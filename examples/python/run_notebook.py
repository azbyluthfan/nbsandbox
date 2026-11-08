# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Another app running a notebook in nbsandbox, end to end.

1. Authenticate: a bearer token (as is, or exchanged on behalf of a user),
   or basic auth in development.
2. Create a space and upload a notebook and its data.
3. Start a run with parameters (safe to retry: Idempotency-Key).
4. Wait for the result with long-polling, then download an artifact.
5. Publish the notebook as a function and call it.
6. Call a function that streams its output (sales_by_month.ipynb): each
   month arrives as soon as it is ready, as a chart would draw it.

Needs Python 3.9+ and requests (pip install requests). Run from the repo:

    export NBSANDBOX_URL=http://127.0.0.1:8080
    export NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password   # or NBSANDBOX_TOKEN=..., or USER_TOKEN=... (see below)
    python examples/python/run_notebook.py --region bandung --months 4
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import sys
import time
import uuid

import requests

HERE = pathlib.Path(__file__).resolve().parent.parent / "notebooks"


# ---------- authentication ----------

def exchange_on_behalf_of(user_token: str) -> tuple[str, float]:
    """Exchanges a signed-in user's token (issued for this app) for one issued
    to nbsandbox, with Entra ID's On-Behalf-Of flow. nbsandbox then sees the user.

    Other providers: OAuth 2.0 Token Exchange (RFC 8693) with
    grant_type=urn:ietf:params:oauth:grant-type:token-exchange,
    subject_token=<user token>, audience=<nbsandbox's audience>.
    """
    tenant = os.environ["AZURE_TENANT_ID"]
    r = requests.post(
        f"https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token",
        data={
            "grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer",
            "requested_token_use": "on_behalf_of",
            "assertion": user_token,
            "client_id": os.environ["APP_CLIENT_ID"],          # this app's registration
            "client_secret": os.environ["APP_CLIENT_SECRET"],
            # nbsandbox's scopes: what this app may do in the user's spaces
            "scope": os.environ.get("NBSANDBOX_SCOPES", "api://nbsandbox/nbsandbox.read api://nbsandbox/nbsandbox.runs"),
        },
        timeout=30,
    )
    r.raise_for_status()
    body = r.json()
    return body["access_token"], time.time() + body.get("expires_in", 300)


class NbsandboxAuth(requests.auth.AuthBase):
    """Adds credentials to each request. A token from the exchange is cached
    until a minute before it expires (don't exchange on every call)."""

    def __init__(self) -> None:
        self.token = os.environ.get("NBSANDBOX_TOKEN")             # already issued for nbsandbox
        self.user_token = os.environ.get("USER_TOKEN")           # a user's token for this app: exchange it
        self.basic = (os.environ.get("NBSANDBOX_USER"), os.environ.get("NBSANDBOX_PASSWORD"))
        self.expires = 0.0

    def __call__(self, r: requests.PreparedRequest) -> requests.PreparedRequest:
        if self.user_token:
            if time.time() > self.expires - 60:
                self.token, self.expires = exchange_on_behalf_of(self.user_token)
            r.headers["Authorization"] = f"Bearer {self.token}"
        elif self.token:
            r.headers["Authorization"] = f"Bearer {self.token}"
        elif all(self.basic):
            return requests.auth.HTTPBasicAuth(*self.basic)(r)
        else:
            sys.exit("set NBSANDBOX_TOKEN, USER_TOKEN (with the exchange settings), or NBSANDBOX_USER and NBSANDBOX_PASSWORD")
        return r


# ---------- calling nbsandbox ----------

class Nbsandbox:
    def __init__(self, url: str) -> None:
        self.base = url.rstrip("/") + "/v1"
        self.s = requests.Session()
        self.s.auth = NbsandboxAuth()

    def call(self, method: str, path: str, **kw) -> requests.Response:
        r = self.s.request(method, self.base + path, timeout=kw.pop("timeout", 70), **kw)
        if r.status_code >= 400:
            err = r.json().get("error", {}) if r.headers.get("Content-Type", "").startswith("application/json") else {}
            # 401: credentials; 403: the token lacks a scope (named in WWW-Authenticate);
            # 400 invalid_parameters: err["problems"] says where the input broke the contract.
            raise RuntimeError(f"{method} {path}: {r.status_code} {err.get('code')}: {err.get('message')} {err.get('problems', '')}")
        return r

    def create_space(self, name: str) -> str:
        return self.call("POST", "/spaces", json={"name": name}).json()["id"]

    def upload(self, space: str, files: list[pathlib.Path], folder: str = "") -> None:
        parts = [("file", (f.name, f.read_bytes())) for f in files]
        self.call("POST", f"/spaces/{space}/files", params={"folder": folder} if folder else None, files=parts)

    def start_run(self, space: str, notebook: str, parameters: dict) -> dict:
        # The same key and body within 24 h return the original run instead of
        # starting another, so a retry after a timeout is safe.
        return self.call("POST", f"/spaces/{space}/runs", json={"notebook": notebook, "parameters": parameters},
                         headers={"Idempotency-Key": str(uuid.uuid4())}).json()

    def wait_result(self, space: str, run_id: str) -> dict:
        """Long-polls: each request waits up to 50 s; 200 = finished, 202 = still going."""
        while True:
            r = self.call("GET", f"/spaces/{space}/runs/{run_id}/result", params={"wait": "50s"})
            if r.status_code == 200:
                return r.json()
            print(f"  still {r.json().get('status')}…")

    def artifact(self, space: str, run_id: str, path: str) -> bytes:
        return self.call("GET", f"/spaces/{space}/runs/{run_id}/artifacts/{path}").content

    def publish(self, space: str, name: str, notebook: str, files: list[str]) -> dict:
        return self.call("POST", f"/spaces/{space}/functions/{name}/versions", json={"notebook": notebook, "files": files}).json()

    def invoke_stream(self, space: str, name: str, parameters: dict, on_item) -> dict:
        """Calls a function and reads its result as Server-Sent Events: one
        "item" event per item as soon as it is made (its id is the item's
        number), then the "result" event. on_item sees each item once."""
        r = self.s.post(self.base + f"/spaces/{space}/functions/{name}/invoke", json={"parameters": parameters},
                        headers={"Accept": "text/event-stream"}, stream=True, timeout=(10, 120))
        r.raise_for_status()
        seen = 0
        event, data = "", ""
        for line in r.iter_lines(decode_unicode=True):
            if line.startswith("event: "):
                event = line[7:]
            elif line.startswith("data: "):
                data += line[6:]
            elif line == "":
                if event == "item":
                    it = json.loads(data)
                    if it["index"] > seen:  # dedupe by index (a resumed stream continues after Last-Event-ID)
                        seen = it["index"]
                        on_item(it)
                elif event == "result":
                    return json.loads(data)
                event, data = "", ""
        raise RuntimeError("the stream ended before the result")

    def invoke(self, space: str, name: str, parameters: dict) -> dict:
        r = self.call("POST", f"/spaces/{space}/functions/{name}/invoke", params={"wait": "50s"}, json={"parameters": parameters})
        res = r.json()
        return res if r.status_code == 200 else self.wait_result(space, res["id"])


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--region", default="jakarta", choices=["jakarta", "bandung", "surabaya"])
    ap.add_argument("--months", type=int, default=3)
    args = ap.parse_args()
    nb = Nbsandbox(os.environ.get("NBSANDBOX_URL", "http://127.0.0.1:8080"))

    space = nb.create_space(f"example {time.strftime('%Y-%m-%d %H:%M:%S')}")
    print(f"space {space}")
    nb.upload(space, [HERE / "sales_report.ipynb"])
    nb.upload(space, [HERE / "data" / "sales.csv"], folder="data")

    params = {"region": args.region, "months": args.months}
    run = nb.start_run(space, "sales_report.ipynb", params)
    print(f"run {run['id']}: {run['status']}")
    res = nb.wait_result(space, run["id"])
    if res["status"] != "succeeded":
        err = res["error"]
        # Retry only what the platform says is retryable (interrupted, platform_error).
        sys.exit(f"run {res['status']}: {err['message']} (retryable: {err['retryable']})")
    print(f"output: {res['output']}")
    print("monthly.csv:\n" + nb.artifact(space, run["id"], "monthly.csv").decode())

    # The same notebook as a function: a frozen copy other services call.
    v = nb.publish(space, "sales-report", "sales_report.ipynb", ["data/sales.csv"])
    print(f"published {v['function']} v{v['version']}")
    res = nb.invoke(space, "sales-report", {"region": "surabaya"})
    print(f"function call {res['status']}: {res.get('output') or res.get('error')}")

    # A function that streams its output: its contract declares a stream of
    # items, and each month arrives as soon as it is computed.
    nb.upload(space, [HERE / "sales_by_month.ipynb"])
    v = nb.publish(space, "sales-by-month", "sales_by_month.ipynb", ["data/sales.csv"])
    print(f"published {v['function']} v{v['version']} ({v['contract']['mode']} output)")
    res = nb.invoke_stream(space, "sales-by-month", {"region": args.region, "months": 6},
                           lambda it: print(f"  month {it['index']} of {it.get('total')}: {it['item']}"))
    if res["status"] != "succeeded":
        # The items that arrived are kept (res["partial"]); call again for the rest.
        sys.exit(f"streamed call {res['status']}: {res['error']['message']} (items so far: {res.get('output')})")
    print(f"streamed call succeeded: {res['items']} items, the same list as output")


if __name__ == "__main__":
    main()
