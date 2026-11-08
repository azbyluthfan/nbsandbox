# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Writes the editor's completion index for this image, gzipped JSON.

Run once while the image is built (so the index matches the installed
packages exactly); nbsandboxd reads the file out of the image and the web
editor completes `pd.` -> `read_csv(...)` from it, with no kernel running.

For each importable module (installed packages, a list of standard-library
modules, nbsandbox, and their public submodules one level down) it records
every public name: its kind, its signature and the first line of its docs.
Names, signatures and docs are cut to a bounded size.

Format:
  {"schema_version": 1, "python_version": "3.13.x",
   "modules": {"pandas": [[name, kind, signature, doc], ...], ...}}
kind: m (module), c (class), f (function), v (anything else).
"""

import gzip
import importlib
import inspect
import io
import json
import pkgutil
import sys
import warnings
import importlib.metadata as md

# Standard-library modules notebooks use most; the rest stay out to keep the
# index small.
STDLIB = [
    "collections", "csv", "datetime", "decimal", "functools", "glob", "itertools", "json", "math", "os", "os.path",
    "pathlib", "random", "re", "shutil", "statistics", "string", "sys", "textwrap", "time", "typing", "urllib.parse",
    "uuid", "zipfile", "io", "hashlib", "base64", "copy", "dataclasses", "enum", "fractions", "heapq", "bisect",
    "operator", "pprint", "logging", "warnings", "contextlib", "tempfile", "gzip", "pickle", "sqlite3",
]
# Packages that are tooling, not something a notebook imports.
SKIP = {"pip", "setuptools", "wheel", "pkg_resources", "_distutils_hack", "papermill", "nbconvert", "ipykernel",
        "jupyter_client", "jupyter_core", "nbclient", "nbformat", "traitlets", "tornado", "zmq", "debugpy",
        "IPython", "ipython_pygments_lexers", "prompt_toolkit", "pygments", "jedi", "parso", "pexpect",
        "ptyprocess", "wcwidth", "pure_eval", "executing", "asttokens", "stack_data", "matplotlib_inline",
        "comm", "nest_asyncio", "psutil", "platformdirs", "jsonschema", "jsonschema_specifications",
        "referencing", "rpds", "fastjsonschema", "attr", "attrs", "bleach", "tinycss2", "webencodings",
        "defusedxml", "mistune", "nbformat", "pandocfilters", "jupyterlab_pygments", "markupsafe", "jinja2",
        "bs4", "soupsieve", "tenacity", "tqdm", "click", "ansicolors", "colors", "entrypoints", "yaml",
        "requests", "urllib3", "idna", "charset_normalizer", "certifi", "decorator", "packaging", "six"}
# Submodules imported on their own (import matplotlib.pyplot as plt): always
# included, ahead of the per-package limit.
PREFERRED = ["matplotlib.pyplot", "matplotlib.dates", "matplotlib.ticker", "numpy.linalg", "numpy.random", "numpy.fft",
             "numpy.ma", "pandas.api", "pandas.plotting", "pandas.tseries", "scipy.stats", "scipy.optimize",
             "scipy.interpolate", "scipy.signal", "scipy.sparse", "sklearn.model_selection", "sklearn.metrics",
             "sklearn.linear_model", "sklearn.preprocessing", "sklearn.ensemble", "sklearn.cluster", "statsmodels.api",
             "dateutil.parser", "dateutil.relativedelta", "PIL.Image", "PIL.ImageDraw"]
MAX_NAMES = 600        # per module
MAX_SIG = 160          # characters
MAX_DOC = 160          # characters
MAX_SUBMODULES = 40    # per package


def first_line(obj):
    try:
        doc = inspect.getdoc(obj) or ""
    except Exception:
        return ""
    for line in doc.splitlines():
        line = line.strip()
        if line:
            return line[:MAX_DOC]
    return ""


def signature(obj):
    try:
        s = str(inspect.signature(obj))
    except Exception:
        return ""
    return s if len(s) <= MAX_SIG else s[:MAX_SIG - 3].rsplit(",", 1)[0] + ", ...)"


def kind(obj):
    if inspect.ismodule(obj):
        return "m"
    if inspect.isclass(obj):
        return "c"
    if callable(obj):
        return "f"
    return "v"


def public_names(mod):
    names = getattr(mod, "__all__", None)
    if not isinstance(names, (list, tuple)):
        names = [n for n in dir(mod) if not n.startswith("_")]
    return sorted({n for n in names if isinstance(n, str) and n.isidentifier() and not n.startswith("_")})


def describe(mod):
    out = []
    for name in public_names(mod)[:MAX_NAMES]:
        try:
            obj = getattr(mod, name)
        except Exception:
            continue
        k = kind(obj)
        out.append([name, k, signature(obj) if k in ("c", "f") else "", first_line(obj) if k != "v" else ""])
    return out


def load(name):
    try:
        with warnings.catch_warnings():
            warnings.simplefilter("ignore")
            return importlib.import_module(name)
    except BaseException:  # some packages raise SystemExit or worse on import
        return None


def top_level():
    names = set()
    for module in md.packages_distributions():
        if module.isidentifier() and not module.startswith("_") and module not in SKIP:
            names.add(module)
    return sorted(names | {"nbsandbox"})


def submodules(pkg):
    path = getattr(pkg, "__path__", None)
    if not path:
        return []
    subs = []
    for info in pkgutil.iter_modules(path):
        n = info.name
        if n.startswith("_") or n in ("tests", "test", "testing", "conftest", "setup"):
            continue
        subs.append(pkg.__name__ + "." + n)
    first = [m for m in PREFERRED if m in subs]
    rest = [m for m in sorted(subs) if m not in first and not m.split(".")[-1].endswith("Plugin")]
    return first + rest[:max(0, MAX_SUBMODULES - len(first))]


def build():
    modules = {}
    for name in STDLIB + top_level():
        mod = load(name)
        if mod is None:
            continue
        modules[name] = describe(mod)
        if name in STDLIB:
            continue
        for sub in submodules(mod):
            m = load(sub)
            if m is not None:
                modules[sub] = describe(m)
    return {"schema_version": 1, "python_version": sys.version.split()[0], "modules": dict(sorted(modules.items()))}


def main():
    out = sys.argv[1] if len(sys.argv) > 1 else "-"
    data = json.dumps(build(), separators=(",", ":"), ensure_ascii=False).encode()
    buf = io.BytesIO()
    with gzip.GzipFile(fileobj=buf, mode="wb", mtime=0) as gz:  # mtime 0: same packages, same bytes
        gz.write(data)
    if out == "-":
        sys.stdout.buffer.write(buf.getvalue())
    else:
        with open(out, "wb") as f:
            f.write(buf.getvalue())
    print(f"completion index: {len(data)} bytes, {len(buf.getvalue())} gzipped", file=sys.stderr)


if __name__ == "__main__":
    main()
