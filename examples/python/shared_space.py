# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Two people working in one space (docs/sharing.md), end to end.

1. The owner creates a space, uploads a notebook and its data, and grants a
   member the write role.
2. The member finds the space among theirs (with their role), runs the
   notebook, and lists the members.
3. The owner sees the member's run (every member sees every run) and lowers
   the member to read: the member can still look, not run.
4. The owner removes the member and deletes the space.

Needs Python 3.9+ and requests (pip install requests). Against the
development stack (make up), from the repo:

    export NBSANDBOX_URL=http://127.0.0.1:8080
    export NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password
    export NBSANDBOX_MEMBER=bob NBSANDBOX_MEMBER_PASSWORD=bob-dev-password
    python examples/python/shared_space.py

With an identity provider, the member is their caller ID (for example
kc:<subject>), and each side sends its own bearer token instead.
"""

from __future__ import annotations

import os
import pathlib
import sys
import time

import requests

HERE = pathlib.Path(__file__).resolve().parent.parent / "notebooks"
URL = os.environ.get("NBSANDBOX_URL", "http://127.0.0.1:8080").rstrip("/")


def session(user: str, password: str) -> requests.Session:
    s = requests.Session()
    s.auth = (user, password)
    return s


def check(r: requests.Response, *ok: int) -> requests.Response:
    if r.status_code not in ok:
        sys.exit(f"{r.request.method} {r.request.url}: {r.status_code} {r.text}")
    return r


def wait(s: requests.Session, space: str, run: str) -> dict:
    while True:
        r = check(s.get(f"{URL}/v1/spaces/{space}/runs/{run}/result", params={"wait": "50s"}), 200, 202)
        if r.status_code == 200:
            return r.json()
        time.sleep(1)


def main() -> None:
    owner_name, member_name = os.environ.get("NBSANDBOX_USER", "alice"), os.environ.get("NBSANDBOX_MEMBER", "bob")
    owner = session(owner_name, os.environ.get("NBSANDBOX_PASSWORD", "alice-dev-password"))
    member = session(member_name, os.environ.get("NBSANDBOX_MEMBER_PASSWORD", "bob-dev-password"))

    # 1. The owner's space, shared with the member.
    space = check(owner.post(f"{URL}/v1/spaces", json={"name": f"shared example {time.strftime('%Y-%m-%d %H:%M:%S')}"}), 201).json()["id"]
    try:
        with open(HERE / "sales_report.ipynb", "rb") as nb:
            check(owner.post(f"{URL}/v1/spaces/{space}/files", files={"file": nb}), 201)
        with open(HERE / "data" / "sales.csv", "rb") as data:
            check(owner.post(f"{URL}/v1/spaces/{space}/files", params={"folder": "data"}, files={"file": data}), 201)
        check(owner.put(f"{URL}/v1/spaces/{space}/members/{member_name}", json={"role": "write"}), 200)
        print(f"{owner_name} shared space {space} with {member_name} (write)")

        # 2. The member: the space is among theirs; they run the notebook.
        theirs = check(member.get(f"{URL}/v1/spaces", params={"role": "write"}), 200).json()["items"]
        mine = next(s for s in theirs if s["id"] == space)
        print(f"{member_name} sees it: role {mine['role']}, created by {mine['owner']}")
        run = check(member.post(f"{URL}/v1/spaces/{space}/runs",
                                json={"notebook": "sales_report.ipynb", "parameters": {"region": "bandung", "months": 4}}), 201).json()
        result = wait(member, space, run["id"])
        print(f"{member_name}'s run: {result['status']}, output {result.get('output')}")
        members = check(member.get(f"{URL}/v1/spaces/{space}/members"), 200).json()["items"]
        print("members:", ", ".join(f"{m['principal']} ({m['role']})" for m in members))

        # 3. The owner sees it; with read, the member can look but not run.
        runs = check(owner.get(f"{URL}/v1/spaces/{space}/runs", params={"owner": member_name}), 200).json()["items"]
        print(f"{owner_name} sees {len(runs)} run(s) by {member_name}")
        check(owner.put(f"{URL}/v1/spaces/{space}/members/{member_name}", json={"role": "read"}), 200)
        refused = member.post(f"{URL}/v1/spaces/{space}/runs", json={"notebook": "sales_report.ipynb"})
        print(f"with read, {member_name} starting a run: {refused.status_code} {refused.json()['error']['message']}")

        # 4. Done: the member leaves the space (or the owner removes them).
        check(member.delete(f"{URL}/v1/spaces/{space}/members/{member_name}"), 204)
        gone = member.get(f"{URL}/v1/spaces/{space}")
        print(f"after leaving, {member_name} reading the space: {gone.status_code}")
    finally:
        owner.delete(f"{URL}/v1/spaces/{space}")


if __name__ == "__main__":
    main()
