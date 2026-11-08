#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Prepares a fresh Ubuntu 24.04 host for nbsandbox. Run as root:
#
#   sudo deploy/provision/provision.sh
#
# What it does (each step is idempotent):
#   1. installs Docker Engine and the Compose plugin from Docker's apt repo
#   2. sets Docker's default log driver to "local" with a size cap
#   3. makes sure uid 1000 exists (on cloud images it is usually "ubuntu")
#   4. creates /srv/nbsandbox (owned by uid 1000, mode 700) and /etc/nbsandbox
#   5. copies the example settings and creates an empty users file
# Afterwards: add users (nbsandboxd hash-password), build or pull the runner
# image, and follow docs/deployment.md.

set -eu

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)"; exit 1; }
. /etc/os-release
[ "${ID:-}" = ubuntu ] || echo "warning: tested on Ubuntu 24.04; this is ${PRETTY_NAME:-unknown}"

REPO_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
DATA_DIR="${NBSANDBOX_DATA_DIR:-/srv/nbsandbox}"

echo "== 1. Docker Engine"
if ! command -v docker >/dev/null 2>&1; then
	apt-get update -q
	apt-get install -yq ca-certificates curl
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
		> /etc/apt/sources.list.d/docker.list
	apt-get update -q
	apt-get install -yq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
docker version --format 'Docker {{.Server.Version}}'

echo "== 2. Docker log defaults"
if [ ! -f /etc/docker/daemon.json ]; then
	cat > /etc/docker/daemon.json <<'JSON'
{
  "log-driver": "local",
  "log-opts": { "max-size": "10m", "max-file": "3" }
}
JSON
	systemctl restart docker
else
	echo "/etc/docker/daemon.json exists; leaving it unchanged (gVisor adds a runtime there, see docs/gvisor.md)"
fi

echo "== 3. uid 1000"
if ! getent passwd 1000 >/dev/null; then
	groupadd --gid 1000 nbsandbox 2>/dev/null || true
	useradd --uid 1000 --gid 1000 --system --no-create-home --shell /usr/sbin/nologin nbsandbox
fi
echo "uid 1000 is $(getent passwd 1000 | cut -d: -f1)"

echo "== 4. Directories"
install -d -m 0700 -o 1000 -g 1000 "$DATA_DIR" "$DATA_DIR/spaces"
install -d -m 0750 -o root -g 1000 /etc/nbsandbox

echo "== 5. Configuration"
[ -f /etc/nbsandbox/settings.yaml ] || install -m 0640 -o root -g 1000 "$REPO_DIR/config/settings.yaml" /etc/nbsandbox/settings.yaml
if [ ! -f /etc/nbsandbox/users.yaml ]; then
	printf '# Add users with: docker run --rm -i ghcr.io/azbyluthfan/nbsandboxd:0.1.0 hash-password\nusers: []\n' > /etc/nbsandbox/users.yaml
	chown 1000:1000 /etc/nbsandbox/users.yaml
	chmod 600 /etc/nbsandbox/users.yaml
fi

echo
echo "Done. Next:"
echo "  - add users to /etc/nbsandbox/users.yaml (nbsandboxd refuses an empty list)"
echo "  - DOCKER_GID=$(getent group docker | cut -d: -f3) goes into deploy/.env"
echo "  - continue with docs/deployment.md"
