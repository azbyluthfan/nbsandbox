# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Working with a notebook's live kernel over the API: an interactive
session (docs/sessions.md), as the web editor does.

1. Create a space with a notebook and its data.
2. Open the notebook's session and connect to its WebSocket: Jupyter
   messages, one JSON object per text message.
3. Run cells; the variables stay between them.
4. Ask the kernel for completions (a DataFrame's columns) and docs.
5. Stop a long cell (interrupt), write a file and save it to the space.
6. End the session.

Needs Python 3.9+, requests and websocket-client
(pip install requests websocket-client). Run from the repo:

    export NBSANDBOX_URL=http://127.0.0.1:8080
    export NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password   # or NBSANDBOX_TOKEN=...
    python examples/python/interactive_session.py
"""

from __future__ import annotations

import base64
import json
import os
import pathlib
import threading
import time
import uuid

import requests
import websocket  # websocket-client

HERE = pathlib.Path(__file__).resolve().parent.parent / "notebooks"
URL = os.environ.get("NBSANDBOX_URL", "http://127.0.0.1:8080").rstrip("/")


def auth_header() -> str:
    if os.environ.get("NBSANDBOX_TOKEN"):
        return "Bearer " + os.environ["NBSANDBOX_TOKEN"]
    pair = f"{os.environ.get('NBSANDBOX_USER', 'alice')}:{os.environ['NBSANDBOX_PASSWORD']}"
    return "Basic " + base64.b64encode(pair.encode()).decode()


API = requests.Session()
API.headers["Authorization"] = auth_header()


def api(method: str, path: str, **kw) -> dict:
    r = API.request(method, URL + "/v1" + path, timeout=300, **kw)
    if r.status_code >= 400:
        raise SystemExit(f"{method} {path}: {r.status_code} {r.text}")
    return r.json() if r.content else {}


class Kernel:
    """A session's WebSocket: send Jupyter messages, collect the answers to
    each request by its msg_id (the session's other clients see them too)."""

    def __init__(self, space: str, session: str):
        ws_url = URL.replace("http", "ws", 1) + f"/v1/spaces/{space}/sessions/{session}/channel"
        self.ws = websocket.create_connection(ws_url, header=[f"Authorization: {auth_header()}"], timeout=180)
        self.inbox: dict[str, list[dict]] = {}
        self.cond = threading.Condition()
        first = json.loads(self.ws.recv())  # the session's state
        print(f"connected: kernel {first['content'].get('kernel')}, {first['content'].get('mode')}")
        threading.Thread(target=self._read, daemon=True).start()

    def _read(self):
        while True:
            try:
                msg = json.loads(self.ws.recv())
            except Exception:
                return
            parent = (msg.get("parent_header") or {}).get("msg_id")
            if msg.get("channel") == "bridge" and msg["content"].get("notice"):
                print("notice:", msg["content"]["notice"])
            with self.cond:
                self.inbox.setdefault(parent or "", []).append(msg)
                self.cond.notify_all()

    def send(self, channel: str, msg_type: str, content: dict) -> str:
        msg_id = uuid.uuid4().hex
        self.ws.send(json.dumps({"channel": channel, "header": {"msg_id": msg_id, "msg_type": msg_type, "session": "example"},
                                 "parent_header": {}, "metadata": {}, "content": content}))
        return msg_id

    def wait(self, msg_id: str, done, timeout: float = 120) -> list[dict]:
        """Messages answering msg_id, until done(msg) is true for one."""
        deadline = time.time() + timeout
        with self.cond:
            while True:
                msgs = self.inbox.get(msg_id, [])
                if any(done(m) for m in msgs):
                    return self.inbox.pop(msg_id)
                left = deadline - time.time()
                if left <= 0:
                    raise TimeoutError(msg_id)
                self.cond.wait(left)

    def run(self, code: str) -> str:
        """Runs a cell; returns its text output (stdout, the result, errors)."""
        msg_id = self.send("shell", "execute_request", {"code": code, "silent": False})
        msgs = self.wait(msg_id, lambda m: m["header"]["msg_type"] == "status" and m["content"]["execution_state"] == "idle")
        out = []
        for m in msgs:
            t, c = m["header"]["msg_type"], m["content"]
            if t == "stream":
                out.append(c["text"])
            elif t == "execute_result":
                out.append(c["data"].get("text/plain", "") + "\n")
            elif t == "error":
                out.append(f"{c['ename']}: {c['evalue']}\n")
        return "".join(out)

    def complete(self, code: str) -> list[str]:
        msg_id = self.send("shell", "complete_request", {"code": code, "cursor_pos": len(code)})
        reply = self.wait(msg_id, lambda m: m["header"]["msg_type"] == "complete_reply", timeout=10)[-1]
        return reply["content"]["matches"]

    def docs(self, code: str) -> str:
        msg_id = self.send("shell", "inspect_request", {"code": code, "cursor_pos": len(code), "detail_level": 0})
        reply = self.wait(msg_id, lambda m: m["header"]["msg_type"] == "inspect_reply", timeout=10)[-1]
        return reply["content"].get("data", {}).get("text/plain", "")

    def close(self):
        self.ws.close()


def main():
    # 1. A space with the notebook and its data.
    space = api("POST", "/spaces", json={"name": f"interactive example {time.strftime('%Y-%m-%d %H:%M')}"})["id"]
    files = [("file", ("sales_report.ipynb", (HERE / "sales_report.ipynb").read_bytes()))]
    api("POST", f"/spaces/{space}/files", files=files)
    api("POST", f"/spaces/{space}/files?folder=data", files=[("file", ("sales.csv", (HERE / "data" / "sales.csv").read_bytes()))])
    print("space", space)

    # 2. The notebook's session (read-write: what the kernel writes goes to
    #    the space), and its WebSocket. The first connect waits for the
    #    kernel (about two seconds).
    s = api("POST", f"/spaces/{space}/sessions", json={"notebook": "sales_report.ipynb"})
    print(f"session {s['id']}: {s['mode']}, {s['status']}")
    k = Kernel(space, s["id"])
    try:
        # 3. Cells; the variables stay between them.
        k.run("import pandas as pd\nsales = pd.read_csv('data/sales.csv')")
        print(k.run("sales.groupby('region')['amount'].sum()"))

        # 4. Completion from the real objects, and docs.
        print("sales[\" completes to:", k.complete('sales["'))
        print("pd.read_csv's docs start:", k.docs("pd.read_csv(").splitlines()[0:2])

        # 5. Stop a long cell; save a file the kernel wrote to the space.
        long = k.send("shell", "execute_request", {"code": "import time\nwhile True: time.sleep(0.1)"})
        time.sleep(1)
        api("POST", f"/spaces/{space}/sessions/{s['id']}/interrupt")
        k.wait(long, lambda m: m["header"]["msg_type"] == "execute_reply")
        print("interrupted the long cell")
        k.run("sales.to_csv('totals.csv', index=False)")
        api("POST", f"/spaces/{space}/sessions/{s['id']}/sync")
        print("totals.csv in the space:", API.get(f"{URL}/v1/spaces/{space}/files/totals.csv", timeout=30).status_code == 200)
    finally:
        # 6. End it (it would end by itself after 30 minutes unused).
        k.close()
        api("DELETE", f"/spaces/{space}/sessions/{s['id']}")
        print("session ended; delete the space when done:", space)


if __name__ == "__main__":
    main()
