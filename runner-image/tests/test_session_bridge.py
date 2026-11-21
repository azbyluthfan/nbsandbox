# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Tests for session_bridge.py with a real kernel, in the runner image:

    make runner-test
"""

import json
import os
import queue
import subprocess
import sys
import threading
import time
import unittest
import uuid

BRIDGE = os.path.join(os.path.dirname(__file__), "..", "session_bridge.py")


class Session:
    def __init__(self, env=None):
        self.p = subprocess.Popen([sys.executable, "-B", BRIDGE], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  env=dict(os.environ, **(env or {})))
        self.q = queue.Queue()
        threading.Thread(target=self._read, daemon=True).start()

    def _read(self):
        for line in self.p.stdout:
            self.q.put(json.loads(line))

    def send(self, channel, msg_type, content=None):
        mid = uuid.uuid4().hex
        msg = {"channel": channel, "header": {"msg_id": mid, "msg_type": msg_type, "session": "test"},
               "parent_header": {}, "metadata": {}, "content": content or {}}
        self.p.stdin.write((json.dumps(msg) + "\n").encode())
        self.p.stdin.flush()
        return mid

    def until(self, pred, timeout=60):
        """Messages up to and including the first that matches pred."""
        seen, deadline = [], time.time() + timeout
        while time.time() < deadline:
            try:
                m = self.q.get(timeout=max(0.01, deadline - time.time()))
            except queue.Empty:
                break
            seen.append(m)
            if pred(m):
                return seen
        raise AssertionError("no matching message; saw %s" % [(m.get("channel"), m.get("header", {}).get("msg_type"),
                                                              m.get("content")) for m in seen][-10:])

    def reply(self, mid, timeout=60):
        return self.until(lambda m: m.get("parent_header", {}).get("msg_id") == mid and m["channel"] in ("shell", "control"),
                          timeout)[-1]

    def execute(self, code):
        mid = self.send("shell", "execute_request", {"code": code, "silent": False})
        msgs = self.until(lambda m: m["channel"] == "iopub" and m["header"]["msg_type"] == "status"
                          and m["parent_header"].get("msg_id") == mid and m["content"]["execution_state"] == "idle")
        return mid, msgs

    def close(self):
        self.p.kill()
        self.p.wait()
        self.p.stdin.close()
        self.p.stdout.close()


def ready(m):
    return m["channel"] == "bridge" and m["content"].get("status") == "ready"


class BridgeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.s = Session(env={"NBSANDBOX_SESSION_MAX_MESSAGE": "200000"})
        cls.s.until(ready, 120)

    @classmethod
    def tearDownClass(cls):
        cls.s.close()

    def test_execute_keeps_state_and_streams_output(self):
        self.s.execute("x = 41")
        _, msgs = self.s.execute("print(x + 1)")
        text = "".join(m["content"]["text"] for m in msgs if m["header"]["msg_type"] == "stream")
        self.assertEqual(text, "42\n")

    def test_complete_knows_live_objects(self):
        self.s.execute("import pandas as pd\ndf = pd.DataFrame({'revenue': [1], 'region': ['a']})")
        mid = self.s.send("shell", "complete_request", {"code": 'df["', "cursor_pos": 4})
        matches = self.s.reply(mid)["content"]["matches"]
        self.assertIn("revenue", matches)
        self.assertIn("region", matches)

    def test_inspect_gives_docs(self):
        mid = self.s.send("shell", "inspect_request", {"code": "len(", "cursor_pos": 4, "detail_level": 0})
        self.assertTrue(self.s.reply(mid)["content"]["found"])

    def test_refuses_other_message_types(self):
        for channel, mt in (("shell", "history_request"), ("shell", "comm_open"), ("stdin", "input_reply")):
            mid = self.s.send(channel, mt)
            m = self.s.until(lambda m: m["channel"] == "bridge" and m["content"].get("msg_id") == mid)[-1]
            self.assertEqual(m["content"]["refused"], mt)

    def test_shutdown_without_restart_is_refused(self):
        mid = self.s.send("control", "shutdown_request", {"restart": False})
        m = self.s.until(lambda m: m["channel"] == "bridge" and m["content"].get("msg_id") == mid)[-1]
        self.assertEqual(m["content"]["refused"], "shutdown_request")

    def test_input_raises(self):
        _, msgs = self.s.execute("input('name? ')")
        errors = [m for m in msgs if m["header"]["msg_type"] == "error"]
        self.assertTrue(errors, msgs)
        self.assertEqual(errors[0]["content"]["ename"], "StdinNotImplementedError")

    def test_interrupt_stops_a_running_cell(self):
        mid = self.s.send("shell", "execute_request", {"code": "import time\nwhile True: time.sleep(0.1)"})
        self.s.until(lambda m: m["channel"] == "iopub" and m["header"]["msg_type"] == "status"
                     and m["parent_header"].get("msg_id") == mid and m["content"]["execution_state"] == "busy")
        imid = self.s.send("control", "interrupt_request")
        self.assertEqual(self.s.reply(imid)["content"]["status"], "ok")
        self.assertEqual(self.s.reply(mid)["content"]["ename"], "KeyboardInterrupt")

    def test_large_output_is_replaced_by_a_notice(self):
        _, msgs = self.s.execute("print('x' * 500000)")
        text = "".join(m["content"]["text"] for m in msgs if m["header"]["msg_type"] == "stream")
        self.assertIn("output not shown", text)
        self.assertLess(len(text), 1000)


class RestartTest(unittest.TestCase):
    def test_restart_forgets_state_and_a_dead_kernel_comes_back(self):
        s = Session()
        try:
            s.until(ready, 120)
            s.execute("y = 1")
            mid = s.send("control", "shutdown_request", {"restart": True})
            self.assertTrue(s.reply(mid, 120)["content"]["restart"])
            s.until(ready, 120)
            _, msgs = s.execute("print('y' in dir())")
            self.assertEqual("".join(m["content"]["text"] for m in msgs if m["header"]["msg_type"] == "stream"), "False\n")
            s.send("shell", "execute_request", {"code": "import os; os._exit(1)"})
            s.until(lambda m: m["channel"] == "bridge" and m["content"].get("status") == "dead", 30)
            s.until(ready, 120)
            _, msgs = s.execute("print(2)")
            self.assertEqual("".join(m["content"]["text"] for m in msgs if m["header"]["msg_type"] == "stream"), "2\n")
        finally:
            s.close()


if __name__ == "__main__":
    unittest.main()
