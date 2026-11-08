# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Unit tests for nbsandbox_runner.py, with papermill and nbconvert
replaced by fakes. Run inside the runner image (stdlib unittest only):

    make runner-test
"""

import json
import os
import sys
import tempfile
import types
import unittest
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import nbsandbox_runner as nr  # noqa: E402

RUN_ID = "11111111-1111-4111-8111-111111111111"


class PapermillExecutionError(Exception):
    def __init__(self, cell_index, exec_count, ename, evalue):
        super().__init__(evalue)
        self.cell_index, self.exec_count, self.ename, self.evalue = cell_index, exec_count, ename, evalue


class DeadKernelError(Exception):
    pass


def install_fakes(execute):
    """Fake papermill and nbclient modules; `execute` plays execute_notebook."""
    return _install_fakes(execute)


class _FakeClient:
    @staticmethod
    def output(self, outs, msg, display_id, cell_index):
        out = {"output_type": "stream", "name": "stdout", "text": msg}
        outs.append(out)
        return out


def _install_fakes(execute):
    """Fake papermill and nbclient modules; `execute` plays execute_notebook."""
    pm = types.ModuleType("papermill")
    pm.__version__ = "fake"
    pm.execute_notebook = execute
    exc = types.ModuleType("papermill.exceptions")
    exc.PapermillExecutionError = PapermillExecutionError
    nbc = types.ModuleType("nbclient")
    nbce = types.ModuleType("nbclient.exceptions")
    nbce.DeadKernelError = DeadKernelError
    nbcc = types.ModuleType("nbclient.client")

    class NotebookClient:
        output = _FakeClient.output

        async def async_execute_cell(self, cell, cell_index, *a, **kw):
            return cell
    nbcc.NotebookClient = NotebookClient
    nbc.client = nbcc
    nbcu = types.ModuleType("nbclient.util")
    nbcu.run_sync = lambda f: f
    nbc.util = nbcu
    return mock.patch.dict(sys.modules, {
        "papermill": pm, "papermill.exceptions": exc, "nbclient": nbc, "nbclient.exceptions": nbce,
        "nbclient.client": nbcc, "nbclient.util": nbcu,
    })


def write_output(cells_tags):
    """execute_notebook stand-in that writes an output notebook."""
    def run(inp, out, **kw):
        cells = [{"cell_type": "code", "metadata": {"tags": t}, "source": "", "outputs": []} for t in cells_tags]
        with open(out, "w") as f:
            json.dump({"nbformat": 4, "cells": cells}, f)
    return run


class RunnerTest(unittest.TestCase):
    def setUp(self):
        self.space = tempfile.mkdtemp()
        self.rundir = tempfile.mkdtemp()
        self.cgroup = tempfile.mkdtemp()
        with open(os.path.join(self.rundir, "input.ipynb"), "w") as f:
            f.write("{}")
        self.env = mock.patch.dict(os.environ, {"NBSANDBOX_SPACE_DIR": self.space, "NBSANDBOX_RUN_DIR": self.rundir,
                                                "NBSANDBOX_RUN_ID": RUN_ID})
        self.env.start()
        self.cg = mock.patch.object(nr, "CGROUP_DIR", self.cgroup)
        self.cg.start()
        self.render = mock.patch.object(nr, "render_html", return_value=(True, None))
        self.render.start()
        nr._status_path = None

    def tearDown(self):
        self.env.stop()
        self.cg.stop()
        self.render.stop()

    @property
    def run_dir(self):
        return self.rundir

    def status(self):
        with open(os.path.join(self.run_dir, ".nbsandbox-status.json")) as f:
            return json.load(f)

    def run_main(self, execute):
        with install_fakes(execute):
            return nr.main(["nbsandbox_runner.py", "nb.ipynb"])

    def test_success(self):
        code = self.run_main(write_output([[]]))
        self.assertEqual(code, nr.EXIT_OK)
        st = self.status()
        self.assertEqual(st["state"], "succeeded")
        self.assertTrue(st["rendered"])
        self.assertEqual(st["schema_version"], 2)
        self.assertTrue(os.path.isdir(os.path.join(self.run_dir, "artifacts")))

    def test_parameters_are_passed(self):
        os.makedirs(self.run_dir, exist_ok=True)
        with open(os.path.join(self.run_dir, ".nbsandbox-params.json"), "w") as f:
            json.dump({"region": "jakarta", "months": 6}, f)
        seen = {}

        def execute(inp, out, **kw):
            seen.update(kw)
            write_output([[]])(inp, out)
        self.run_main(execute)
        self.assertEqual(seen["parameters"], {"region": "jakarta", "months": 6})
        self.assertEqual(seen["kernel_name"], "python3")
        self.assertTrue(seen["log_output"])
        self.assertTrue(seen["request_save_on_cell_execute"])

    def test_parameters_must_be_object(self):
        os.makedirs(self.run_dir, exist_ok=True)
        with open(os.path.join(self.run_dir, ".nbsandbox-params.json"), "w") as f:
            json.dump([1, 2], f)
        self.assertEqual(self.run_main(write_output([[]])), nr.EXIT_WRAPPER_ERROR)
        self.assertEqual(self.status()["state"], "wrapper_error")

    def test_execution_error_maps_cell_index_past_injected_cell(self):
        def execute(inp, out, **kw):
            # parameters cell, injected cell, then user cells 1..3; error in output cell 4
            write_output([["parameters"], ["injected-parameters"], [], [], []])(inp, out)
            raise PapermillExecutionError(4, 4, "ValueError", "boom")
        self.assertEqual(self.run_main(execute), nr.EXIT_EXEC_ERROR)
        st = self.status()
        self.assertEqual(st["state"], "execution_error")
        self.assertEqual(st["error"], {"cell_index": 3, "execution_count": 4, "ename": "ValueError", "evalue": "boom"})

    def test_cell_index_ignores_papermill_error_cells(self):
        # What papermill really saves after a failure: an error note at the
        # top and an anchor before the failing cell, both tagged
        # papermill-error-cell-tag. The exception's index (2: parameters,
        # injected, failing cell) counts cells before they were added.
        err = ["papermill-error-cell-tag"]
        def execute(inp, out, **kw):
            write_output([err, ["parameters"], ["injected-parameters"], err, [], []])(inp, out)
            raise PapermillExecutionError(2, 2, "ValueError", "asked to fail")
        self.run_main(execute)
        self.assertEqual(self.status()["error"]["cell_index"], 1)

    def test_cell_index_with_error_cells_and_no_parameters_cell(self):
        # No parameters cell: papermill injects at the top.
        err = ["papermill-error-cell-tag"]
        def execute(inp, out, **kw):
            write_output([err, ["injected-parameters"], [], [], err, []])(inp, out)
            raise PapermillExecutionError(3, 3, "ValueError", "x")
        self.run_main(execute)
        self.assertEqual(self.status()["error"]["cell_index"], 2)

    def test_error_before_injected_cell_keeps_index(self):
        def execute(inp, out, **kw):
            write_output([[], ["injected-parameters"], []])(inp, out)
            raise PapermillExecutionError(0, 1, "NameError", "x")
        self.run_main(execute)
        self.assertEqual(self.status()["error"]["cell_index"], 0)

    def test_dead_kernel(self):
        def execute(inp, out, **kw):
            raise DeadKernelError("Kernel died")
        self.assertEqual(self.run_main(execute), nr.EXIT_DEAD_KERNEL)
        self.assertEqual(self.status()["state"], "dead_kernel")

    def test_other_failure_is_wrapper_error(self):
        def execute(inp, out, **kw):
            raise RuntimeError("kernel failed to start")
        self.assertEqual(self.run_main(execute), nr.EXIT_WRAPPER_ERROR)
        st = self.status()
        self.assertEqual(st["state"], "wrapper_error")
        self.assertIn("kernel failed to start", st["message"])

    def test_bad_run_id(self):
        with mock.patch.dict(os.environ, {"NBSANDBOX_RUN_ID": "../../etc"}):
            self.assertEqual(self.run_main(write_output([[]])), nr.EXIT_WRAPPER_ERROR)
        self.assertFalse(os.path.exists(os.path.join(self.run_dir, "artifacts")))

    def test_render_failure_keeps_success(self):
        with mock.patch.object(nr, "render_html", return_value=(False, "nbconvert failed")):
            self.assertEqual(self.run_main(write_output([[]])), nr.EXIT_OK)
        st = self.status()
        self.assertEqual(st["state"], "succeeded")
        self.assertFalse(st["rendered"])
        self.assertEqual(st["render_error"], "nbconvert failed")

    def test_cgroup_counters(self):
        files = {"memory.events": "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\n", "memory.peak": "1073741824\n",
                 "memory.max": "2147483648\n", "pids.events": "max 2\n", "cpu.stat": "usage_usec 3100000\nuser_usec 1\n"}
        for name, body in files.items():
            with open(os.path.join(self.cgroup, name), "w") as f:
                f.write(body)
        self.run_main(write_output([[]]))
        self.assertEqual(self.status()["cgroup"], {
            "oom": 1, "oom_kill": 1, "memory_peak_bytes": 1073741824, "memory_max_bytes": 2147483648,
            "pids_max_events": 2, "cpu_usage_usec": 3100000})

    def test_missing_cgroup_files_are_null(self):
        self.run_main(write_output([[]]))
        self.assertIsNone(self.status()["cgroup"]["memory_peak_bytes"])

    def test_stored_output_is_capped(self):
        wrapped, state = nr.make_capped_output(_FakeClient.output, limit=200)
        outs = []
        returned = [wrapped(None, outs, "x" * 40, None, 0) for _ in range(10)]
        self.assertTrue(all(r is not None for r in returned), "every output still goes to the log")
        stored = [o for o in outs if "nbsandbox" not in o["text"]]
        note = [o for o in outs if "not stored" in o["text"]][0]
        self.assertEqual(note.output_type, "stream", "the note must be a NotebookNode (attribute access)")
        self.assertLess(sum(len(json.dumps(o)) for o in stored), 200)
        self.assertEqual(sum("not stored" in o["text"] for o in outs), 1, "one note, once")
        self.assertGreater(state["dropped"], 0)
        # Outputs with a display id are never dropped (their index must stay valid).
        before = len(outs)
        wrapped(None, outs, "y" * 400, "disp-1", 0)
        self.assertEqual(len(outs), before + 1)

    def test_log_filter(self):
        import logging
        rec = logging.LogRecord("papermill", logging.INFO, "", 0, "hello\n", None, None)
        self.assertTrue(nr._clean_log_record(rec))
        self.assertEqual(rec.getMessage(), "hello")
        for noise in ["\n", "   ", "x Kernel is running over TCP without encryption y"]:
            self.assertFalse(nr._clean_log_record(logging.LogRecord("p", 20, "", 0, noise, None, None)))


class CellEventsTest(unittest.TestCase):
    def test_cells_are_reported_in_the_uploaded_numbering(self):
        import asyncio
        path = os.path.join(tempfile.mkdtemp(), "events.jsonl")
        cells = [
            {"cell_type": "markdown"},
            {"cell_type": "code", "metadata": {"tags": ["parameters"]}},
            {"cell_type": "code", "metadata": {"tags": ["injected-parameters"]}},
            {"cell_type": "code"},
            {"cell_type": "code"},
        ]

        async def orig(self, cell, index, *a, **kw):
            if index == 4:
                raise RuntimeError("cell failed")
            return "ran"

        run = nr.make_cell_events(orig, path)
        client = types.SimpleNamespace(nb=types.SimpleNamespace(cells=cells))
        for i, c in enumerate(cells):
            if i == 4:
                with self.assertRaises(RuntimeError):
                    asyncio.run(run(client, c, i))
            else:
                asyncio.run(run(client, c, i))
        with open(path) as f:
            events = [json.loads(line) for line in f]
        got = [(e["cell_index"], e["state"], e.get("outcome")) for e in events]
        # Markdown and the injected cell aren't reported; cells after the
        # injected one keep their uploaded index (3 -> 2, 4 -> 3).
        self.assertEqual(got, [(1, "started", None), (1, "finished", "ok"), (2, "started", None), (2, "finished", "ok"),
                               (3, "started", None), (3, "finished", "error")])
        self.assertIn("duration_seconds", events[1])

    def test_append_event_never_raises(self):
        nr.append_event("/nonexistent/dir/events.jsonl", {"kind": "cell"})


if __name__ == "__main__":
    unittest.main()


class KernelStartupTest(unittest.TestCase):
    """kernel_startup.py: no output history; stdout/stderr bounded at twice the log cap."""

    def test_output_history_keeps_nothing(self):
        from IPython.core.interactiveshell import InteractiveShell
        shell = InteractiveShell.instance()
        path = os.path.join(os.path.dirname(__file__), "..", "kernel_startup.py")
        with open(path) as f:
            exec(compile(f.read(), path, "exec"), {})
        outputs = shell.history_manager.outputs
        outputs[1].append("printed")
        self.assertNotIn(1, outputs)
        self.assertEqual(outputs[1], [])

    def run_startup(self, cap):
        sent = {"out": [], "err": []}

        class Stream:
            def __init__(self, key):
                self.key = key

            def write(self, s):
                sent[self.key].append(s)
                return len(s)

        out, err = Stream("out"), Stream("err")
        path = os.path.join(os.path.dirname(__file__), "..", "kernel_startup.py")
        with mock.patch.dict(os.environ, {"NBSANDBOX_LOG_CAP": cap}), \
                mock.patch.object(sys, "stdout", out), mock.patch.object(sys, "stderr", err):
            ns = {}
            with open(path) as f:
                exec(compile(f.read(), path, "exec"), ns)
            self.assertNotIn("_nbsandbox_bound_output", ns)  # leaves nothing in the user's namespace
            for _ in range(10):
                self.assertEqual(out.write("x" * 10), 10)
                self.assertEqual(err.write("y" * 10), 10)
        return sent

    def test_drops_past_twice_the_cap(self):
        sent = self.run_startup("50")  # budget 100 characters, shared by both streams
        self.assertEqual(sum(map(len, sent["out"] + sent["err"])), 100)

    def test_no_cap_no_change(self):
        for cap in ("", "0", "junk"):
            sent = self.run_startup(cap)
            self.assertEqual(sum(map(len, sent["out"] + sent["err"])), 200)

    def test_installed_in_ipythondir(self):
        with tempfile.TemporaryDirectory() as d, mock.patch.dict(os.environ, {"IPYTHONDIR": d}):
            nr.install_kernel_startup()
            with open(os.path.join(d, "profile_default", "startup", "00-nbsandbox.py")) as f:
                self.assertIn("_nbsandbox_bound_output", f.read())


class FullWritesTest(unittest.TestCase):
    """FullWrites retries short writes (gVisor returns them on large writes)."""

    def test_short_writes_are_retried(self):
        got = []

        def short_write(fd, data):
            chunk = bytes(data[:3])  # at most 3 bytes a call
            got.append(chunk)
            return len(chunk)

        with mock.patch.object(nr.os, "write", side_effect=short_write):
            s = nr.FullWrites(2)
            self.assertEqual(s.write("héllo wörld\n"), 12)
        self.assertEqual(b"".join(got).decode(), "héllo wörld\n")
        self.assertGreater(len(got), 3)
        self.assertEqual(s.fileno(), 2)


class CGroupV1Test(unittest.TestCase):
    """gVisor emulates cgroup v1 with few files: no peak, so it is sampled."""

    def test_v1_fallbacks_and_sampled_peak(self):
        with tempfile.TemporaryDirectory() as d:
            for sub, name, value in [("memory", "memory.usage_in_bytes", "1000"), ("memory", "memory.limit_in_bytes", "4096"),
                                     ("cpuacct", "cpuacct.usage", "5000000")]:
                os.makedirs(os.path.join(d, sub), exist_ok=True)
                with open(os.path.join(d, sub, name), "w") as f:
                    f.write(value)
            with mock.patch.object(nr, "CGROUP_DIR", d):
                self.assertTrue(nr.needs_sampling())
                sampler = nr.MemorySampler(interval=0.01)
                sampler.sample()
                with open(os.path.join(d, "memory", "memory.usage_in_bytes"), "w") as f:
                    f.write("3000")
                sampler.sample()
                with open(os.path.join(d, "memory", "memory.usage_in_bytes"), "w") as f:
                    f.write("2000")
                with mock.patch.object(nr, "_sampler", sampler):
                    c = nr.cgroup_counters()
            self.assertEqual(c["memory_peak_bytes"], 3000)
            self.assertTrue(c["memory_peak_sampled"])
            self.assertEqual(c["memory_max_bytes"], 4096)
            self.assertEqual(c["cpu_usage_usec"], 5000)

    def test_v2_peak_needs_no_sampling(self):
        with tempfile.TemporaryDirectory() as d:
            for name, value in [("memory.peak", "7000"), ("memory.current", "10")]:
                with open(os.path.join(d, name), "w") as f:
                    f.write(value)
            with mock.patch.object(nr, "CGROUP_DIR", d):
                self.assertFalse(nr.needs_sampling())
                c = nr.cgroup_counters()
            self.assertEqual(c["memory_peak_bytes"], 7000)
            self.assertNotIn("memory_peak_sampled", c)


class NprocTest(unittest.TestCase):
    """NBSANDBOX_NPROC sets RLIMIT_NPROC (the process limit inside a sandbox)."""

    def test_applied_only_when_set(self):
        with mock.patch.object(nr.resource, "setrlimit") as setrlimit:
            with mock.patch.dict(os.environ, {"NBSANDBOX_NPROC": "256"}):
                nr.apply_nproc()
            setrlimit.assert_called_once_with(nr.resource.RLIMIT_NPROC, (256, 256))
            setrlimit.reset_mock()
            for v in ("", "0", "junk"):
                with mock.patch.dict(os.environ, {"NBSANDBOX_NPROC": v}):
                    nr.apply_nproc()
            setrlimit.assert_not_called()

    def test_sampled_peak_capped_at_limit(self):
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(os.path.join(d, "memory"))
            for name, value in [("memory.usage_in_bytes", "9000"), ("memory.limit_in_bytes", "4096")]:
                with open(os.path.join(d, "memory", name), "w") as f:
                    f.write(value)
            with mock.patch.object(nr, "CGROUP_DIR", d):
                sampler = nr.MemorySampler()
                with mock.patch.object(nr, "_sampler", sampler):
                    c = nr.cgroup_counters()
            self.assertEqual(c["memory_peak_bytes"], 4096)
