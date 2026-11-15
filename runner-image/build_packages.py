# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Builds a space's package set. Runs in the builder image
(nbsandbox-runner-builder: the runner's exact packages, plus pip), in a
container with network access to the package index and nothing else of
nbsandbox's: no space files, no credentials.

In:  /in/request.json  {"index_url", "requirements", "from_lock", "prefer_lock"}
Out: /out/site-packages  the packages installed (only those the runner image
                         doesn't have; the runner's own are constraints)
     /out/result.json    {"ok", "error", "lock", "installed", "used"}

Wheels only (--only-binary=:all:): installing a wheel runs no package code,
so pip is the only program that runs here. The output (pip's) is the build
log nbsandboxd shows."""

import json
import os
import shutil
import subprocess
import sys
import venv

ENV = "/tmp/env"
OUT = "/out"


def pip(*args):
    return subprocess.run([sys.executable, "-m", "pip", "--disable-pip-version-check", "--no-input", *args])


def site_packages(env):
    return os.path.join(env, "lib", f"python{sys.version_info.major}.{sys.version_info.minor}", "site-packages")


def lock_of(report_path):
    """The exact packages installed, as requirement lines with hashes."""
    with open(report_path) as f:
        report = json.load(f)
    lines, installed = [], []
    for item in report.get("install", []):
        meta = item.get("metadata", {})
        name, version = meta.get("name", ""), meta.get("version", "")
        sha = item.get("download_info", {}).get("archive_info", {}).get("hashes", {}).get("sha256", "")
        if not name or not version:
            continue
        line = f"{name}=={version}"
        if sha:
            line += f" --hash=sha256:{sha}"
        lines.append(line)
        installed.append({"name": name, "version": version})
    lines.sort(key=str.lower)
    installed.sort(key=lambda d: d["name"].lower())
    return "\n".join(lines) + ("\n" if lines else ""), installed


def attempt(name, text, exact, index_url, constraints):
    """One pip install into a fresh environment that sees the runner's
    packages (so they count as installed). True if it worked."""
    print(f"== installing from {name}", flush=True)
    shutil.rmtree(ENV, ignore_errors=True)
    venv.create(ENV, system_site_packages=True, with_pip=False)
    with open("/tmp/requirements.txt", "w") as f:
        f.write(text)
    args = ["--python", os.path.join(ENV, "bin", "python"), "install", "--only-binary=:all:", "--progress-bar", "off",
            "--index-url", index_url, "-r", "/tmp/requirements.txt", "--report", "/tmp/report.json"]
    if exact:
        # A lock: exactly these files, nothing resolved. pip check below
        # tells whether they still fit the runner image.
        args += ["--require-hashes", "--no-deps"]
    else:
        args += ["-c", constraints]
    if pip(*args).returncode != 0:
        return False
    check = subprocess.run([os.path.join(ENV, "bin", "python"), "-m", "pip", "check"], capture_output=True, text=True)
    if check.returncode != 0 and exact:
        print("the lock no longer fits the runner image:\n" + check.stdout, flush=True)
        return False
    return True


def main():
    with open("/in/request.json") as f:
        req = json.load(f)
    frozen = subprocess.run([sys.executable, "-m", "pip", "list", "--format=freeze", "--exclude", "pip"],
                            capture_output=True, text=True, check=True).stdout
    constraints = "/tmp/constraints.txt"
    with open(constraints, "w") as f:
        f.write(frozen)

    attempts = []
    if req.get("from_lock"):
        attempts.append(("the published version's lock", req["requirements"], True))
    else:
        if req.get("prefer_lock"):
            attempts.append(("the space's last lock", req["prefer_lock"], True))
        attempts.append(("requirements.txt", req["requirements"], False))

    result = {"ok": False, "error": "", "lock": "", "installed": [], "used": ""}
    for name, text, exact in attempts:
        if attempt(name, text, exact, req["index_url"], constraints):
            result["lock"], result["installed"] = lock_of("/tmp/report.json")
            result["ok"], result["used"] = True, name
            break
        if name == "the space's last lock":
            print("== resolving requirements.txt afresh", flush=True)
    else:
        result["error"] = ("pip couldn't install the requirements: see the build log (a package without a wheel for this "
                           "platform, a version that doesn't exist, or one that conflicts with the runner image's packages)")

    if result["ok"]:
        shutil.copytree(site_packages(ENV), os.path.join(OUT, "site-packages"), symlinks=True)
    with open(os.path.join(OUT, "result.json"), "w") as f:
        json.dump(result, f)
    print("== " + ("done" if result["ok"] else "failed"), flush=True)


if __name__ == "__main__":
    main()
