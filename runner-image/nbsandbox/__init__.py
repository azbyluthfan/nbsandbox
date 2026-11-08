# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""nbsandbox: a notebook's side of the input/output contract.

    import nbsandbox

    params = nbsandbox.parameters()        # the caller's JSON, exactly as sent
    ...
    nbsandbox.output({"forecast": forecast, "model": {"mape": 0.07}})

parameters() returns the run's parameters as parsed JSON. papermill also
injects each top-level key as a variable in the cell after the one tagged
"parameters"; use parameters() when you need the exact JSON (big integers,
key order, or keys you didn't declare).

output(value) records the value the run returns to its caller. It accepts any
JSON-serialisable value and converts the usual data-science types:

    numpy numbers and booleans      -> numbers and booleans
    numpy arrays                    -> (nested) lists
    pandas DataFrame                -> list of records ({"column": value, ...})
    pandas Series, Index            -> lists
    datetime, date, time, Timestamp -> ISO 8601 strings
    Decimal                         -> numbers
    tuples, sets                    -> lists
    NaN, Infinity                   -> null (JSON has no NaN)

Anything else (bytes, arbitrary objects, dict keys that aren't strings or
numbers) raises TypeError in the calling cell, so the run fails with a clear
traceback. Calling output() again replaces the earlier value. The value is
only returned to the caller if the run succeeds.

A notebook returns its result in one of two ways, fixed by its contract
(or, without one, by which of the two it calls):

    nbsandbox.output(value)          # one value
    nbsandbox.emit(item)             # a stream: call once per item, as each is ready
    nbsandbox.emit(item, total=8)    # ... optionally saying how many to expect

A streamed output's items reach callers following the run as soon as each
is emitted, and the run's result is the list of them. Calling the other
function than the run's mode raises in that cell. Items convert like
output() values, at most 64 KiB each, and emit() waits briefly rather than
send more than 10 a second.

    nbsandbox.progress(0.4, "3 of 8 regions")   # 0.0 to 1.0, for callers watching

progress() never affects the result.
"""

import datetime as _dt
import decimal as _decimal
import json as _json
import math as _math
import os as _os
import time as _time

__all__ = ["parameters", "output", "emit", "progress"]

_MAX_DEPTH = 64


def _run_dir():
    return _os.environ.get("NBSANDBOX_RUN_DIR", "/run")


def _max_output():
    try:
        return int(_os.environ.get("NBSANDBOX_MAX_OUTPUT", str(1 << 20)))
    except ValueError:
        return 1 << 20


def parameters():
    """The run's parameters as a dict (empty if none were sent)."""
    path = _os.path.join(_run_dir(), ".nbsandbox-params.json")
    try:
        with open(path, encoding="utf-8") as f:
            value = _json.load(f)
    except FileNotFoundError:
        return {}
    return value if isinstance(value, dict) else {}


def _jsonable(v, depth=0):
    if depth > _MAX_DEPTH:
        raise ValueError(f"nbsandbox.output: value is nested deeper than {_MAX_DEPTH} levels")
    if v is None or isinstance(v, (bool, str)):
        return v
    if isinstance(v, int):
        return v
    if isinstance(v, float):
        return v if _math.isfinite(v) else None
    if isinstance(v, _decimal.Decimal):
        return _jsonable(float(v), depth) if v.is_finite() else None
    if isinstance(v, (_dt.datetime, _dt.date, _dt.time)):
        return v.isoformat()
    if isinstance(v, dict):
        out = {}
        for k, x in v.items():
            if isinstance(k, bool) or not isinstance(k, (str, int, float)):
                raise TypeError(f"nbsandbox.output: dict keys must be strings or numbers, got {type(k).__name__}")
            out[str(k)] = _jsonable(x, depth + 1)
        return out
    if isinstance(v, (list, tuple, set, frozenset)):
        return [_jsonable(x, depth + 1) for x in v]

    # numpy and pandas, without importing them unless the value is one.
    mod = type(v).__module__ or ""
    if mod.startswith("numpy"):
        if hasattr(v, "tolist"):  # ndarray and numpy scalars
            return _jsonable(v.tolist(), depth)
    if mod.startswith("pandas"):
        name = type(v).__name__
        if name == "DataFrame":
            return [_jsonable(r, depth + 1) for r in v.to_dict(orient="records")]
        if name in ("Series", "Index", "DatetimeIndex", "RangeIndex"):
            return _jsonable(v.tolist(), depth)
        if name == "Timestamp":
            return v.isoformat()
        if name in ("NaTType", "NAType"):
            return None
        if name == "Timedelta":
            return v.isoformat()
    raise TypeError(f"nbsandbox.output: can't convert {type(v).__module__}.{type(v).__name__} to JSON; "
                    "convert it to numbers, strings, lists or dicts first")


def _mode():
    return _os.environ.get("NBSANDBOX_OUTPUT_MODE", "single")


def output(value):
    """Record value as the run's result for the caller (replaces any earlier one)."""
    if _mode() == "stream":
        raise RuntimeError("nbsandbox.output: this notebook returns a stream of items (its contract, or its calls to "
                           "nbsandbox.emit, say so); send each item with nbsandbox.emit instead")
    data = _json.dumps(_jsonable(value), ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode("utf-8")
    limit = _max_output()
    if len(data) > limit:
        raise ValueError(f"nbsandbox.output: the value is {len(data)} bytes; the limit is {limit}. "
                         "Write large results to a file in NBSANDBOX_OUTPUT_DIR (an artifact) instead")
    run_dir = _run_dir()
    tmp = _os.path.join(run_dir, ".nbsandbox-output.tmp")
    with open(tmp, "wb") as f:
        f.write(data)
    _os.replace(tmp, _os.path.join(run_dir, "output.json"))


# One JSON line per event, read by nbsandboxd while the run works.
_EVENTS_FILE = ".nbsandbox-events.jsonl"
_MAX_EVENT = 64 << 10


def _append_event(event):
    line = _json.dumps(event, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode("utf-8") + b"\n"
    if len(line) > _MAX_EVENT:
        raise ValueError(f"nbsandbox: an event is {len(line)} bytes; the limit is {_MAX_EVENT}. "
                         "Emit a summary, or write the details to a file in NBSANDBOX_OUTPUT_DIR")
    fd = _os.open(_os.path.join(_run_dir(), _EVENTS_FILE), _os.O_WRONLY | _os.O_APPEND | _os.O_CREAT, 0o600)
    try:
        _os.write(fd, line)  # one write: whole lines, even with the runner writing too
    finally:
        _os.close(fd)


_ITEMS_PER_SECOND = 10
_last_emit = [0.0]


def emit(item, total=None):
    """Send one item of the run's output: callers following the run get it now,
    and the result is the list of items. total, if given, is how many to expect."""
    if _mode() != "stream":
        raise RuntimeError("nbsandbox.emit: this notebook returns one value (its contract says so); "
                           "return it with nbsandbox.output, or declare {\"mode\": \"stream\"} in its contract")
    if total is not None and (isinstance(total, bool) or not isinstance(total, int) or total < 1):
        raise ValueError("nbsandbox.emit: total must be a positive whole number")
    event = {"kind": "item", "item": _jsonable(item)}
    if total is not None:
        event["total"] = total
    # Pace to the rate nbsandboxd accepts, rather than have items refused.
    wait = _last_emit[0] + 1 / _ITEMS_PER_SECOND - _time.monotonic()
    if wait > 0:
        _time.sleep(wait)
    _append_event(event)
    _last_emit[0] = _time.monotonic()


def progress(fraction, message=""):
    """Report how far the run is (0.0 to 1.0), with an optional message."""
    if isinstance(fraction, bool) or not isinstance(fraction, (int, float)) or not 0 <= fraction <= 1:
        raise ValueError("nbsandbox.progress: fraction must be a number from 0 to 1")
    if not isinstance(message, str):
        raise TypeError("nbsandbox.progress: message must be a string")
    _append_event({"kind": "progress", "fraction": float(fraction), "message": message[:1000]})
