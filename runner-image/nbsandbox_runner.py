# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""nbsandbox execution wrapper (runs inside the runner container).

Runs one notebook with papermill, renders it to HTML, reads this container's
own cgroup counters, and writes a machine-readable status file. nbsandboxd uses
the status file to classify the run.

Usage:
    nbsandbox_runner.py <notebook name, for messages only>

Environment:
    NBSANDBOX_RUN_ID      run UUID
    NBSANDBOX_RUN_DIR     this run's folder (default /run); the only folder
                        outputs are written to
    NBSANDBOX_SPACE_DIR   the space's files, the working directory (default
                        /space; read-only for function invocations)
    NBSANDBOX_NPROC       if set, RLIMIT_NPROC for the run: the process limit
                        inside a sandbox with its own kernel (gVisor on
                        Kubernetes), where the node's pids limit can't be used
    NBSANDBOX_LOG_CAP     bytes of log nbsandboxd keeps; the kernel stops
                        sending stdout/stderr past twice this (kernel_startup.py)

Files in the run directory:
    input.ipynb            input: the notebook as it was when the run was submitted
    .nbsandbox-params.json   input: papermill parameters (optional)
    output.json            the value the notebook returned with nbsandbox.output
    output.ipynb           executed notebook, saved after every cell
    output.html            rendered notebook (if rendering worked)
    .nbsandbox-status.json   output: this wrapper's report
    .nbsandbox-events.jsonl  output: cell starts and ends, and the notebook's
                             nbsandbox.emit() / progress() calls, as they happen

Exit codes:
    0   notebook executed successfully (rendering may still have failed)
    10  a cell raised an exception
    11  the kernel died
    12  wrapper error (bad input, papermill failure outside a cell)
    143 stopped by SIGTERM
"""

import io
import json
import logging
import os
import re
import resource
import signal
import subprocess
import sys
import tempfile
import threading
import time

STATUS_SCHEMA_VERSION = 2  # 2: outputs in NBSANDBOX_RUN_DIR, input is input.ipynb
EXIT_OK, EXIT_EXEC_ERROR, EXIT_DEAD_KERNEL, EXIT_WRAPPER_ERROR = 0, 10, 11, 12
CGROUP_DIR = "/sys/fs/cgroup"

_RUN_ID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")

_status_path = None
_started = time.time()


def _now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _read_kv(path):
    """Parse a cgroup 'key value' file into a dict of ints; None if unreadable."""
    try:
        out = {}
        with open(path) as f:
            for line in f:
                parts = line.split()
                if len(parts) == 2:
                    out[parts[0]] = int(parts[1])
        return out
    except (OSError, ValueError):
        return None


def _read_int(path):
    try:
        with open(path) as f:
            return int(f.read().strip())
    except (OSError, ValueError):
        return None


def _memory_now():
    """Current memory use of this container: cgroup v2, else v1 (gVisor
    emulates v1 directories with only a few files)."""
    v = _read_int(os.path.join(CGROUP_DIR, "memory.current"))
    return v if v is not None else _read_int(os.path.join(CGROUP_DIR, "memory", "memory.usage_in_bytes"))


class MemorySampler:
    """Samples memory use when the cgroup keeps no peak (gVisor has neither
    memory.peak nor memory.max_usage_in_bytes). A sampled peak can miss a
    spike shorter than the interval; the status says it was sampled."""

    def __init__(self, interval=0.2):
        self.peak = None
        self._interval = interval
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name="memory-sampler", daemon=True)

    def sample(self):
        v = _memory_now()
        if v is not None and (self.peak is None or v > self.peak):
            self.peak = v

    def _run(self):
        while not self._stop.is_set():
            self.sample()
            self._stop.wait(self._interval)

    def start(self):
        self._thread.start()
        return self

    def stop(self):
        self._stop.set()
        self.sample()


_sampler = None  # a MemorySampler, while one runs


def needs_sampling():
    return (_read_int(os.path.join(CGROUP_DIR, "memory.peak")) is None
            and _read_int(os.path.join(CGROUP_DIR, "memory", "memory.max_usage_in_bytes")) is None
            and _memory_now() is not None)


def cgroup_counters():
    mem_events = _read_kv(os.path.join(CGROUP_DIR, "memory.events")) or {}
    pids_events = _read_kv(os.path.join(CGROUP_DIR, "pids.events")) or {}
    cpu_stat = _read_kv(os.path.join(CGROUP_DIR, "cpu.stat")) or {}
    peak = _read_int(os.path.join(CGROUP_DIR, "memory.peak"))
    if peak is None:  # cgroup v1
        peak = _read_int(os.path.join(CGROUP_DIR, "memory", "memory.max_usage_in_bytes"))
    sampled = False
    if peak is None and _sampler is not None:
        _sampler.sample()
        peak, sampled = _sampler.peak, _sampler.peak is not None
    cpu = cpu_stat.get("usage_usec")
    if cpu is None:  # cgroup v1: nanoseconds
        ns = _read_int(os.path.join(CGROUP_DIR, "cpuacct", "cpuacct.usage"))
        cpu = ns // 1000 if ns is not None else None
    limit = _read_int(os.path.join(CGROUP_DIR, "memory.max"))
    if limit is None:
        limit = _read_int(os.path.join(CGROUP_DIR, "memory", "memory.limit_in_bytes"))
    if sampled and limit is not None and peak > limit:
        peak = limit  # gVisor's usage counter can read above the limit
    out = {
        "oom": mem_events.get("oom"),
        "oom_kill": mem_events.get("oom_kill"),
        "memory_peak_bytes": peak,
        "memory_max_bytes": limit,
        "pids_max_events": pids_events.get("max"),
        "cpu_usage_usec": cpu,
    }
    if sampled:
        out["memory_peak_sampled"] = True
    return out


def write_status(state, **fields):
    status = {
        "schema_version": STATUS_SCHEMA_VERSION,
        "run_id": os.environ.get("NBSANDBOX_RUN_ID"),
        "state": state,
        "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(_started)),
        "finished_at": _now(),
        "python_version": sys.version.split()[0],
        "cgroup": cgroup_counters(),
    }
    status.update(fields)
    if _status_path is None:
        print(json.dumps(status), file=sys.stderr)
        return
    tmp = _status_path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(status, f, indent=2)
    os.replace(tmp, _status_path)


class FullWrites(io.TextIOBase):
    """A text stream on a file descriptor that writes everything it is given.

    Under gVisor a write to the container's stdout or stderr pipe can return
    after 64 KiB, and Python's unbuffered streams (PYTHONUNBUFFERED) don't
    retry short writes: the rest of a large log record was silently lost.
    This keeps writing until every byte is out (blocking while the pipe is
    full, which is what a pipe should do)."""

    def __init__(self, fd):
        super().__init__()
        self._fd = fd

    def write(self, s):
        view = memoryview(s.encode("utf-8", "backslashreplace"))
        while view:
            n = os.write(self._fd, view)
            view = view[n:]
        return len(s)

    def fileno(self):
        return self._fd

    def writable(self):
        return True


def apply_nproc():
    """Sets RLIMIT_NPROC from NBSANDBOX_NPROC, for this process and the
    kernel it starts. nbsandboxd sets it only inside a sandbox with its own
    kernel; elsewhere the limit would count every process of uid 1000."""
    try:
        n = int(os.environ.get("NBSANDBOX_NPROC", "0"))
    except ValueError:
        return
    if n > 0:
        resource.setrlimit(resource.RLIMIT_NPROC, (n, n))


def _on_sigterm(signum, frame):
    # The service is stopping this run (cancel, timeout or shutdown). Record
    # what we know and exit at once; the output notebook was saved per cell.
    try:
        write_status("terminated")
    finally:
        os._exit(128 + signum)


def _original_cell_index(output_nb_path, cell_index):
    """papermill inserts an 'injected-parameters' cell. Map an index in the
    executed notebook back to the user's notebook (0-based).

    The exception's index counts cells before papermill adds its error cells
    (tagged papermill-error-cell-tag: one at the top, one before the failing
    cell) to the saved output, so those are skipped when locating the
    injected cell."""
    if cell_index is None:
        return None
    try:
        with open(output_nb_path) as f:
            cells = json.load(f).get("cells", [])
        cells = [c for c in cells if "papermill-error-cell-tag" not in c.get("metadata", {}).get("tags", [])]
        for i, cell in enumerate(cells):
            tags = cell.get("metadata", {}).get("tags", [])
            if "injected-parameters" in tags:
                return cell_index - 1 if cell_index > i else cell_index
    except (OSError, ValueError):
        pass
    return cell_index


# Outputs kept in the executed notebook, per run. Past this, outputs still go
# to the live log (which has its own cap) but are no longer stored in the
# notebook: papermill keeps the whole notebook in memory and rewrites it after
# every cell, so endless output would otherwise end in an out-of-memory kill
# and an unbounded output.ipynb.
MAX_STORED_OUTPUT = int(os.environ.get("NBSANDBOX_MAX_STORED_OUTPUT", 5 * 1024 * 1024))


def make_capped_output(orig, limit):
    """Wraps nbclient's NotebookClient.output so stored outputs stop at
    `limit` bytes; returns (wrapper, state)."""
    state = {"bytes": 0, "dropped": 0, "noted": False}

    def output(self, outs, msg, display_id, cell_index):
        before = len(outs)
        out = orig(self, outs, msg, display_id, cell_index)
        if out is None or len(outs) <= before or display_id:
            return out  # nothing stored, or an updatable display (keep its index valid)
        size = len(json.dumps(out, default=str))
        if state["bytes"] + size <= limit:
            state["bytes"] += size
            return out
        outs.pop()
        state["dropped"] += 1
        if not state["noted"]:
            state["noted"] = True
            from nbformat.v4 import new_output  # a NotebookNode, as nbclient expects
            outs.append(new_output("stream", name="stderr",
                                   text=f"[nbsandbox] further output is not stored in this notebook (over {limit // (1024 * 1024)} MiB); see the run log\n"))
        return out  # still returned, so papermill writes it to the log

    return output, state


def append_event(path, event):
    """Appends one JSON line to the run's events file (nbsandboxd reads it
    while the run works). One write, so lines never interleave with the
    notebook's own nbsandbox.emit() calls."""
    line = (json.dumps(event, separators=(",", ":")) + "\n").encode()
    try:
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
        try:
            os.write(fd, line)
        finally:
            os.close(fd)
    except OSError:
        pass  # events are a courtesy; never fail the run over one


def _tags(cell):
    return cell.get("metadata", {}).get("tags", [])


def make_cell_events(orig, events_path):
    """Wraps nbclient's NotebookClient.async_execute_cell to report when each
    code cell starts and finishes, numbered as in the uploaded notebook (the
    cell papermill injects for the parameters is neither reported nor
    counted)."""
    async def execute_cell(self, cell, cell_index, *args, **kwargs):
        if cell.get("cell_type") != "code" or "injected-parameters" in _tags(cell):
            return await orig(self, cell, cell_index, *args, **kwargs)
        index = cell_index - sum(1 for c in self.nb.cells[:cell_index] if "injected-parameters" in _tags(c))
        append_event(events_path, {"kind": "cell", "cell_index": index, "state": "started"})
        start, outcome = time.monotonic(), "error"
        try:
            result = await orig(self, cell, cell_index, *args, **kwargs)
            outcome = "ok"
            return result
        finally:
            append_event(events_path, {"kind": "cell", "cell_index": index, "state": "finished", "outcome": outcome,
                                       "duration_seconds": round(time.monotonic() - start, 3)})

    return execute_cell


# Known log noise that is not useful to the notebook's author.
_NOISE = ("Kernel is running over TCP without encryption",)


def _ipc_kernel_manager():
    """A kernel manager that talks to the kernel over Unix sockets instead of
    TCP on loopback. Over TCP, ipykernel prints a "running over TCP without
    encryption" warning straight to stderr (so into every run log), and the
    kernel's ports would be open to every process in the container. The
    sockets go in /tmp, never in the user's space (the working directory)."""
    from jupyter_client.manager import AsyncKernelManager

    class IPCKernelManager(AsyncKernelManager):
        def __init__(self, **kwargs):
            kwargs.setdefault("transport", "ipc")
            kwargs.setdefault("ip", os.path.join(tempfile.mkdtemp(prefix="nbsandbox-kernel-"), "kernel"))
            super().__init__(**kwargs)

    return IPCKernelManager


def _clean_log_record(record):
    """Drop the trailing newline papermill keeps on each output chunk (it
    produced an empty line after every line) and known kernel noise."""
    msg = record.getMessage().rstrip("\n")
    if not msg.strip() or any(n in msg for n in _NOISE):
        return False
    record.msg, record.args = msg, None
    return True


KERNEL_STARTUP = os.path.join(os.path.dirname(os.path.abspath(__file__)), "kernel_startup.py")


def install_kernel_startup():
    """Installs kernel_startup.py as an IPython startup file, so it runs in
    the notebook's kernel before the first cell (it bounds the kernel's
    output; see the file). $IPYTHONDIR is in /tmp, empty at every start."""
    if not os.path.exists(KERNEL_STARTUP):
        return
    startup = os.path.join(os.environ.get("IPYTHONDIR", os.path.expanduser("~/.ipython")), "profile_default", "startup")
    os.makedirs(startup, exist_ok=True)
    with open(KERNEL_STARTUP) as src, open(os.path.join(startup, "00-nbsandbox.py"), "w") as dst:
        dst.write(src.read())


def render_html(output_nb, output_html):
    proc = subprocess.run(
        [sys.executable, "-m", "nbconvert", "--to", "html",
         "--output", os.path.basename(output_html), output_nb],
        capture_output=True, text=True, timeout=300,
    )
    if proc.returncode != 0:
        return False, proc.stderr.strip()[-2000:]
    return True, None


def main(argv):
    global _status_path, _sampler
    signal.signal(signal.SIGTERM, _on_sigterm)
    apply_nproc()
    if needs_sampling():
        _sampler = MemorySampler().start()

    space = os.environ.get("NBSANDBOX_SPACE_DIR", "/space")
    run_id = os.environ.get("NBSANDBOX_RUN_ID", "")
    if len(argv) != 2 or not _RUN_ID_RE.match(run_id):
        write_status("wrapper_error", message="usage: nbsandbox_runner.py <notebook>; NBSANDBOX_RUN_ID must be a UUID")
        return EXIT_WRAPPER_ERROR

    run_dir = os.environ.get("NBSANDBOX_RUN_DIR", "/run")
    os.makedirs(os.path.join(run_dir, "artifacts"), exist_ok=True)
    _status_path = os.path.join(run_dir, ".nbsandbox-status.json")
    input_nb = os.path.join(run_dir, "input.ipynb")
    output_nb = os.path.join(run_dir, "output.ipynb")
    output_html = os.path.join(run_dir, "output.html")

    params = {}
    params_path = os.path.join(run_dir, ".nbsandbox-params.json")
    if os.path.exists(params_path):
        try:
            with open(params_path) as f:
                params = json.load(f)
        except (OSError, ValueError) as e:
            write_status("wrapper_error", message=f"cannot read parameters: {e}")
            return EXIT_WRAPPER_ERROR
        if not isinstance(params, dict):
            write_status("wrapper_error", message="parameters must be a JSON object")
            return EXIT_WRAPPER_ERROR

    # papermill logs each line of cell output through the "papermill" logger
    # as the kernel produces it (log_output=True). Plain lines on stderr.
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(logging.Formatter("%(message)s"))
    handler.addFilter(_clean_log_record)
    logging.basicConfig(level=logging.INFO, handlers=[handler])

    install_kernel_startup()

    import papermill
    from papermill.exceptions import PapermillExecutionError
    try:
        from nbclient.exceptions import DeadKernelError
    except ImportError:  # pragma: no cover
        DeadKernelError = ()

    import nbclient.client
    capped, out_state = make_capped_output(nbclient.client.NotebookClient.output, MAX_STORED_OUTPUT)
    nbclient.client.NotebookClient.output = capped
    cell_events = make_cell_events(nbclient.client.NotebookClient.async_execute_cell,
                                   os.path.join(run_dir, ".nbsandbox-events.jsonl"))
    nbclient.client.NotebookClient.async_execute_cell = cell_events
    # execute_cell (what papermill calls) is a sync wrapper made when the
    # class was defined, around the original: wrap ours the same way.
    from nbclient.util import run_sync
    nbclient.client.NotebookClient.execute_cell = run_sync(cell_events)

    common = {"papermill_version": papermill.__version__}
    state, code, extra = "succeeded", EXIT_OK, {}
    try:
        papermill.execute_notebook(
            input_nb, output_nb,
            parameters=params,
            kernel_name="python3",          # ignore whatever kernel the upload names
            cwd=space,
            log_output=True,
            progress_bar=False,
            request_save_on_cell_execute=True,
            kernel_manager_class=_ipc_kernel_manager(),
        )
    except PapermillExecutionError as e:
        state, code = "execution_error", EXIT_EXEC_ERROR
        extra["error"] = {
            "cell_index": _original_cell_index(output_nb, e.cell_index),
            "execution_count": e.exec_count,
            "ename": e.ename,
            "evalue": str(e.evalue)[:4000],
        }
    except DeadKernelError as e:
        state, code = "dead_kernel", EXIT_DEAD_KERNEL
        extra["message"] = str(e)[:4000]
    except Exception as e:  # papermill failed outside a cell (bad notebook, kernel start, ...)
        state, code = "wrapper_error", EXIT_WRAPPER_ERROR
        extra["message"] = f"{type(e).__name__}: {e}"[:4000]

    rendered, render_error = False, None
    if os.path.exists(output_nb):
        try:
            rendered, render_error = render_html(output_nb, output_html)
        except Exception as e:
            render_error = f"{type(e).__name__}: {e}"
    extra["rendered"] = rendered
    if render_error:
        extra["render_error"] = render_error

    if out_state["dropped"]:
        extra["outputs_not_stored"] = out_state["dropped"]
    write_status(state, **common, **extra)
    return code


if __name__ == "__main__":
    # The run log is this process's stderr: never lose part of a write.
    sys.stdout, sys.stderr = FullWrites(1), FullWrites(2)
    sys.exit(main(sys.argv))
