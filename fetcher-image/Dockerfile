# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# nbsandbox fetcher image: holds a run's credentials and reads its sources
# with DuckDB, serving the results to the run over Arrow Flight
# (docs/plan-data-access.md). Build: make fetcher-image
#
# python:3.13-slim, pinned by multi-arch index digest (amd64 + arm64).
FROM python:3.13-slim@sha256:09a17cad02668ae61ece136b4fd36ac3849e14f670ebedb637ea3e15580525bc

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1

COPY requirements.txt /tmp/requirements.txt
# Extensions are installed here, at build time: the fetcher never installs
# or downloads anything at run time (autoinstall and autoload are off).
RUN apt-get update && apt-get upgrade -y --no-install-recommends && rm -rf /var/lib/apt/lists/* \
 && pip install --root-user-action=ignore --require-hashes --no-deps -r /tmp/requirements.txt \
 && rm /tmp/requirements.txt \
 && python -c "import duckdb; c = duckdb.connect(config={'extension_directory': '/opt/nbsandbox-fetcher/extensions'}); [c.execute(f'INSTALL {e}') for e in ('postgres', 'httpfs')]" \
 && pip uninstall -y --root-user-action=ignore pip \
 && rm -rf /usr/local/lib/python3.13/ensurepip \
 && find / -xdev -type f -perm /6000 -exec chmod a-s {} + \
 && groupadd --gid 1000 nbsandbox \
 && useradd --uid 1000 --gid 1000 --no-create-home --home-dir /tmp --shell /usr/sbin/nologin nbsandbox

COPY fetcher.py /opt/nbsandbox-fetcher/

# The root filesystem is read-only at run time; /tmp is a tmpfs.
ENV HOME=/tmp \
    PYTHONUNBUFFERED=1 \
    NBSANDBOX_FETCHER_CONFIG=/etc/nbsandbox-fetcher/config.json
USER 1000:1000
WORKDIR /tmp
EXPOSE 8815
ENTRYPOINT ["python", "/opt/nbsandbox-fetcher/fetcher.py"]
