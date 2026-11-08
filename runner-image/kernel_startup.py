# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Runs in the notebook's kernel as it starts (an IPython startup file the
runner installs in $IPYTHONDIR). Keeps a notebook that prints without pause
from running out of memory through its own output:

- IPython (9+) keeps a copy of everything each cell prints, as MIME bundles in
  its output history, with no limit. A run never reads that history (the
  runner gets output from the kernel's messages), so it keeps nothing.
- Past twice NBSANDBOX_LOG_CAP, the kernel stops sending stdout and stderr:
  nbsandboxd keeps only the first NBSANDBOX_LOG_CAP bytes of a run's log, so
  nothing that would have been kept is lost, and the queue between the kernel
  and the runner (which reads more slowly than a print loop writes) stays
  small. The notebook keeps running."""


def _nbsandbox_bound_output():
    import os
    import sys

    class NoOutputs(dict):
        """An output history that keeps nothing."""

        def __contains__(self, key):
            return False

        def __getitem__(self, key):
            return []

        def __setitem__(self, key, value):
            pass

    try:
        from IPython import get_ipython

        shell = get_ipython()
        if shell is not None and hasattr(shell, "history_manager") and hasattr(shell.history_manager, "outputs"):
            shell.history_manager.outputs = NoOutputs()
    except Exception:
        pass

    try:
        cap = int(os.environ.get("NBSANDBOX_LOG_CAP", "0"))
    except ValueError:
        return
    if cap <= 0:
        return
    budget = 2 * cap
    sent = [0]
    for stream in (sys.stdout, sys.stderr):
        write = getattr(stream, "write", None)
        if write is None:
            continue

        def bounded(s, *args, _write=write, **kwargs):
            if sent[0] >= budget:
                return len(s)
            sent[0] += len(s)
            return _write(s, *args, **kwargs)

        try:
            stream.write = bounded
        except (AttributeError, TypeError):
            pass


_nbsandbox_bound_output()
del _nbsandbox_bound_output
