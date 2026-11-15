# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# nbsandbox runner image: the only environment notebooks run in.
# Build: make runner-image

# The builder image (target "builder"): the runner's exact packages (the
# same hash-pinned requirements) plus pip, for building spaces' package sets
# (build_packages.py). Only build containers use it; runs never do.
# python:3.13-slim, pinned by multi-arch index digest (amd64 + arm64).
FROM python:3.13-slim@sha256:09a17cad02668ae61ece136b4fd36ac3849e14f670ebedb637ea3e15580525bc AS builder
ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1
COPY requirements.txt builder-requirements.txt /tmp/
RUN apt-get update && apt-get upgrade -y --no-install-recommends && rm -rf /var/lib/apt/lists/* \
 && pip install --root-user-action=ignore --require-hashes --no-deps -r /tmp/builder-requirements.txt \
 && pip install --root-user-action=ignore --require-hashes --no-deps -r /tmp/requirements.txt \
 && rm /tmp/requirements.txt /tmp/builder-requirements.txt \
 && rm -rf /usr/local/lib/python3.13/ensurepip \
 && find / -xdev -type f -perm /6000 -exec chmod a-s {} + \
 && groupadd --gid 1000 nbsandbox \
 && useradd --uid 1000 --gid 1000 --no-create-home --home-dir /tmp --shell /usr/sbin/nologin nbsandbox
COPY build_packages.py /opt/nbsandbox/
ENV HOME=/tmp
USER 1000:1000
WORKDIR /tmp
ENTRYPOINT ["python", "/opt/nbsandbox/build_packages.py"]

# The runner image (the default target).
# python:3.13-slim, pinned by multi-arch index digest (amd64 + arm64).
FROM python:3.13-slim@sha256:09a17cad02668ae61ece136b4fd36ac3849e14f670ebedb637ea3e15580525bc AS runner

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1

COPY requirements.txt /tmp/requirements.txt
RUN apt-get update && apt-get upgrade -y --no-install-recommends && rm -rf /var/lib/apt/lists/* \
 && pip install --root-user-action=ignore --require-hashes --no-deps -r /tmp/requirements.txt \
 && rm /tmp/requirements.txt \
 # Runs have no network, so pip is of no use there: removed, with the
 # copies of urllib3, msgpack and setuptools it bundles (scanners flag them).
 && pip uninstall -y --root-user-action=ignore pip \
 && rm -rf /usr/local/lib/python3.13/ensurepip \
 # Hardening: no setuid/setgid binaries in the image.
 && find / -xdev -type f -perm /6000 -exec chmod a-s {} + \
 && groupadd --gid 1000 nbsandbox \
 && useradd --uid 1000 --gid 1000 --no-create-home --home-dir /tmp --shell /usr/sbin/nologin nbsandbox

COPY nbsandbox_runner.py export_manifest.py kernel_startup.py /opt/nbsandbox/
# The nbsandbox module notebooks import (nbsandbox.output, nbsandbox.parameters),
# on every Python's path through a .pth file.
COPY nbsandbox/ /opt/nbsandbox/lib/nbsandbox/
RUN echo /opt/nbsandbox/lib > "$(python -c 'import sysconfig; print(sysconfig.get_paths()["purelib"])')/nbsandbox.pth"

# The editor's completion index: every public name of the importable
# modules, with signatures and short docs, built from exactly these
# packages. nbsandboxd reads it out of the image for the web editor.
COPY export_completions.py /opt/nbsandbox/
RUN HOME=/tmp MPLCONFIGDIR=/tmp/matplotlib python /opt/nbsandbox/export_completions.py /opt/nbsandbox/completions.json.gz \
 && rm -rf /tmp/matplotlib

# The root filesystem is read-only at run time; everything writable lives in
# /tmp (a tmpfs), /run (this run's folder) or /space (the space's files,
# read-only for function invocations).
ENV HOME=/tmp \
    PYTHONUNBUFFERED=1 \
    MPLCONFIGDIR=/tmp/matplotlib \
    IPYTHONDIR=/tmp/ipython \
    JUPYTER_RUNTIME_DIR=/tmp/jupyter/runtime \
    JUPYTER_CONFIG_DIR=/tmp/jupyter/config \
    JUPYTER_PLATFORM_DIRS=1

USER 1000:1000
WORKDIR /space
ENTRYPOINT ["python", "/opt/nbsandbox/nbsandbox_runner.py"]
