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

    def test_notebooks_written_for_runs_work(self):
        _, msgs = self.s.execute("import os, nbsandbox\n"
                                 "open(os.path.join(os.environ['NBSANDBOX_OUTPUT_DIR'], 'x.csv'), 'w').write('a')\n"
                                 "nbsandbox.output({'total': 3})\nnbsandbox.progress(0.5, 'half')")
        text = "".join(m["content"]["text"] for m in msgs if m["header"]["msg_type"] == "stream")
        self.assertIn('"total": 3', text)
        self.assertIn("50% half", text)
        self.assertNotIn("Error", text)

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



class PumpTest(unittest.TestCase):
    """The pump keeps relaying whatever happens on a poll."""

    def test_pump_polls_again_and_survives_errors(self):
        import asyncio
        import io

        sys.path.insert(0, os.path.dirname(BRIDGE))
        import session_bridge

        reply = {"header": {"msg_type": "complete_reply", "msg_id": "r"}, "parent_header": {"msg_id": "q"},
                 "metadata": {}, "content": {"matches": ["a"]}}

        class Channel:
            def __init__(self):
                self.calls = []

            async def get_msg(self, timeout=None):
                self.calls.append(timeout)
                n = len(self.calls)
                if n == 1:
                    raise queue.Empty  # nothing within the timeout: poll again
                if n == 2:
                    raise RuntimeError("boom")  # reported; the pump goes on
                if n == 3:
                    return reply
                await asyncio.sleep(3600)

        async def run():
            out = io.BytesIO()
            b = session_bridge.Bridge(out=out)
            ch = Channel()
            old, session_bridge.PUMP_POLL = session_bridge.PUMP_POLL, 0.05
            try:
                task = asyncio.create_task(b.pump(ch, "shell"))
                for _ in range(100):
                    if len(ch.calls) >= 4:
                        break
                    await asyncio.sleep(0.01)
                task.cancel()
                try:
                    await task
                except asyncio.CancelledError:
                    pass
            finally:
                session_bridge.PUMP_POLL = old
            return ch.calls, [json.loads(line) for line in out.getvalue().splitlines()]

        calls, msgs = asyncio.run(run())
        self.assertTrue(all(t is not None for t in calls), calls)  # never an endless poll
        self.assertIn("boom", msgs[0]["content"]["notice"])
        self.assertEqual((msgs[1]["channel"], msgs[1]["header"]["msg_type"]), ("shell", "complete_reply"))



class InterruptTimingTest(unittest.TestCase):
    """An interrupt is never lost: the kernel ignores SIGINT between saying
    "busy" and starting the cell's code (it says execute_input then)."""

    def run_case(self, steps, hold=0.2):
        import asyncio
        import io

        sys.path.insert(0, os.path.dirname(BRIDGE))
        import session_bridge

        class KM:
            def __init__(self):
                self.interrupts = 0

            async def interrupt_kernel(self):
                self.interrupts += 1

        def iopub(mt, parent, **content):
            return {"header": {"msg_type": mt}, "parent_header": {"msg_id": parent, "msg_type": "execute_request"},
                    "content": content}

        async def run():
            b = session_bridge.Bridge(out=io.BytesIO())
            b.km = KM()
            old, session_bridge.INTERRUPT_HOLD = session_bridge.INTERRUPT_HOLD, hold
            seen = []
            try:
                for step in steps:
                    if step == "interrupt":
                        await b.interrupt()
                    elif step == "wait":
                        await asyncio.sleep(hold * 2)
                    elif step == "busy":
                        await b.track(iopub("status", "c1", execution_state="busy"))
                    elif step == "idle":
                        await b.track(iopub("status", "c1", execution_state="idle"))
                    else:
                        await b.track(iopub(step, "c1"))
                    seen.append(b.km.interrupts)
            finally:
                session_bridge.INTERRUPT_HOLD = old
            return seen

        return asyncio.run(run())

    def test_interrupt_before_the_code_starts_waits_for_it(self):
        self.assertEqual(self.run_case(["busy", "interrupt", "execute_input"]), [0, 0, 1])

    def test_a_cell_that_never_says_it_started_is_interrupted_after_the_hold(self):
        self.assertEqual(self.run_case(["busy", "interrupt", "wait"]), [0, 0, 1])

    def test_interrupt_of_running_code_is_at_once(self):
        self.assertEqual(self.run_case(["busy", "execute_input", "interrupt", "wait"]), [0, 0, 1, 1])

    def test_a_cell_that_ends_before_it_starts_is_not_interrupted_later(self):
        self.assertEqual(self.run_case(["busy", "interrupt", "idle", "wait"]), [0, 0, 0, 0])

    def test_interrupt_with_no_cell_running_is_at_once(self):
        self.assertEqual(self.run_case(["interrupt"]), [1])


class LostReplyTest(unittest.TestCase):
    """An interrupt caught by ipykernel's dispatcher (between execute_input
    and the cell's code) leaves the cell idle without an execute_reply: the
    bridge supplies one, once."""

    def run_case(self, real_reply):
        import asyncio
        import io

        sys.path.insert(0, os.path.dirname(BRIDGE))
        import session_bridge

        class KM:
            async def interrupt_kernel(self):
                pass

        parent = {"msg_id": "c1", "msg_type": "execute_request", "session": "s"}

        def iopub(mt, **content):
            return {"header": {"msg_type": mt}, "parent_header": parent, "content": content}

        reply = {"header": {"msg_type": "execute_reply"}, "parent_header": parent, "content": {"status": "ok"}}

        async def run():
            out = io.BytesIO()
            b = session_bridge.Bridge(out=out)
            b.km = KM()
            old, session_bridge.REPLY_GRACE = session_bridge.REPLY_GRACE, 0.1
            try:
                await b.track(iopub("status", execution_state="busy"))
                await b.track(iopub("execute_input"))
                await b.interrupt()
                await b.track(iopub("status", execution_state="idle"))
                passed = None
                if real_reply == "in time":
                    passed = b.replied(reply)
                await asyncio.sleep(0.3)
                if real_reply == "late":
                    passed = b.replied(reply)
            finally:
                session_bridge.REPLY_GRACE = old
            return passed, [json.loads(line) for line in out.getvalue().splitlines()]

        return asyncio.run(run())

    def test_a_lost_reply_is_supplied(self):
        _, msgs = self.run_case(None)
        self.assertEqual(len(msgs), 1, msgs)
        m = msgs[0]
        self.assertEqual((m["channel"], m["header"]["msg_type"], m["parent_header"]["msg_id"]), ("shell", "execute_reply", "c1"))
        self.assertEqual((m["content"]["status"], m["content"]["ename"]), ("error", "KeyboardInterrupt"))

    def test_the_kernels_reply_in_time_means_none_is_supplied(self):
        passed, msgs = self.run_case("in time")
        self.assertTrue(passed)
        self.assertEqual(msgs, [])

    def test_a_late_reply_after_a_supplied_one_is_dropped(self):
        passed, msgs = self.run_case("late")
        self.assertFalse(passed)
        self.assertEqual(len(msgs), 1)


if __name__ == "__main__":
    unittest.main()
