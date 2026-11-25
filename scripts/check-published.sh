#!/usr/bin/env bash
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Deploys a published release as docs/deployment.md does, with the images
# users pull from GHCR, and runs notebooks through it: the only check of the
# published images (every other test builds them from source). Runs in the
# weekly workflow and before announcing a release (docs/releasing.md).
#
#   scripts/check-published.sh [VERSION]    # e.g. 0.3.0; default: the newest v* tag
#
# Uses that release's own Compose file, settings and client from its tag, a
# throwaway data directory and users file, and a Compose project of its own
# (nbsandbox-published), on ports 18081 and 18443 (NBSANDBOX_HTTP_PORT,
# NBSANDBOX_HTTPS_PORT). Removes everything it made when it ends. On Linux it
# uses sudo to give the data directory and users file to uid 1000, as the
# provisioning script does. Needs Docker with Compose, git, curl, openssl,
# and python3 with venv (the session check, for releases with sessions).

set -euo pipefail

cd "$(dirname "$0")/.."
REPO=$PWD
VERSION=${1:-}
if [ -z "$VERSION" ]; then
	git fetch -q --tags origin 2>/dev/null || true
	VERSION=$(git tag --list 'v*' --sort=-v:refname | head -n 1)
fi
VERSION=${VERSION#v}
[ -n "$VERSION" ] || { echo "check-published: no release tag found" >&2; exit 1; }
git rev-parse -q --verify "refs/tags/v$VERSION" >/dev/null || { echo "check-published: no tag v$VERSION (git fetch --tags)" >&2; exit 1; }

REG=ghcr.io/azbyluthfan
HTTPS_PORT=${NBSANDBOX_HTTPS_PORT:-18443}
HTTP_PORT=${NBSANDBOX_HTTP_PORT:-18081}
PROJECT=nbsandbox-published
WORK=$(mktemp -d "${TMPDIR:-/tmp}/nbsandbox-published.XXXXXX")
WORK=$(cd "$WORK" && pwd -P) # the data directory's path must be the same on the host and in nbsandboxd
SRC=$WORK/src
linux=false
[ "$(uname -s)" = Linux ] && linux=true

step() { printf '\n== %s\n' "$*"; }
compose() { docker compose -p "$PROJECT" -f "$SRC/deploy/docker-compose.yml" --env-file "$SRC/deploy/.env" "$@"; }

cleanup() {
	status=$?
	if [ $status -ne 0 ] && [ -f "$SRC/deploy/.env" ] && [ -n "$(compose ps -aq 2>/dev/null)" ]; then
		step "FAILED: the stack's logs"
		compose logs --no-color 2>&1 | tail -150 || true
		step "FAILED: nbsandboxd's readiness checks"
		compose exec -T nbsandboxd /nbsandboxd health 2>&1 || true
	fi
	if [ -f "$SRC/deploy/.env" ]; then compose down -v --remove-orphans >/dev/null 2>&1 || true; fi
	git -C "$REPO" worktree remove --force "$SRC" >/dev/null 2>&1 || true
	if $linux; then sudo rm -rf "$WORK"; else rm -rf "$WORK"; fi
	exit $status
}
trap cleanup EXIT

step "Release v$VERSION, from its tag"
git worktree add -q --detach "$SRC" "v$VERSION"
mkdir -p "$WORK/srv" "$WORK/etc"
chmod 700 "$WORK/srv" # as the provisioning script makes it; preflight checks it on Linux

step "Published images (anonymous pulls: they must be public)"
# An empty Docker config has no registry logins; keep the current daemon.
host=${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}
for image in nbsandboxd nbsandbox-runner nbsandbox-runner-builder nbsandbox-fetcher; do
	DOCKER_CONFIG=$WORK/no-login DOCKER_HOST=$host docker pull -q "$REG/$image:$VERSION"
done

step "A user, with the release's hash-password"
password=$(openssl rand -hex 16)
hash=$(printf '%s\n%s\n' "$password" "$password" | docker run --rm -i "$REG/nbsandboxd:$VERSION" hash-password 2>/dev/null | tail -n 1)
case "$hash" in \$2*) ;; *) echo "hash-password printed no bcrypt hash" >&2; exit 1 ;; esac
printf 'users:\n  - username: alice\n    password_hash: "%s"\n' "$hash" > "$WORK/etc/users.yaml"
chmod 600 "$WORK/etc/users.yaml"
cp "$SRC/config/settings.yaml" "$WORK/etc/settings.yaml"
chmod 644 "$WORK/etc/settings.yaml"
if $linux; then
	sudo chown -R 1000:1000 "$WORK/srv" "$WORK/etc/users.yaml"
	gid=$(getent group docker | cut -d: -f3)
else
	gid=0
fi

cat > "$SRC/deploy/.env" <<EOF
NBSANDBOX_DOMAIN=localhost
NBSANDBOX_TLS=internal
NBSANDBOX_HTTP_PORT=$HTTP_PORT
NBSANDBOX_HTTPS_PORT=$HTTPS_PORT
DOCKER_GID=$gid
NBSANDBOX_DB_PASSWORD=$(openssl rand -hex 24)
NBSANDBOX_S3_SECRET_KEY=$(openssl rand -hex 24)
NBSANDBOX_DATA_DIR=$WORK/srv
NBSANDBOX_SETTINGS=$WORK/etc/settings.yaml
NBSANDBOX_USERS=$WORK/etc/users.yaml
NBSANDBOX_IMAGE=$REG/nbsandboxd:$VERSION
NBSANDBOX_RUNNER_IMAGE=$REG/nbsandbox-runner:$VERSION
NBSANDBOX_PACKAGES_ENABLED=true
EOF

step "Preflight"
NBSANDBOX_DATA_DIR=$WORK/srv NBSANDBOX_USERS_FILE=$WORK/etc/users.yaml \
	NBSANDBOX_RUNNER_IMAGE=$REG/nbsandbox-runner:$VERSION "$SRC/scripts/preflight.sh"

step "Start (waits for nbsandboxd to be ready)"
compose up -d --wait --quiet-pull

step "Through Caddy's HTTPS, with the release's client"
compose cp caddy:/data/caddy/pki/authorities/local/root.crt "$WORK/root.crt" >/dev/null
export CURL_CA_BUNDLE=$WORK/root.crt NBSANDBOX_URL=https://localhost:$HTTPS_PORT NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=$password
client=$SRC/scripts/nbsandbox.sh
runtime=$("$client" runtime)
case "$runtime" in *"\"ref\":\"$REG/nbsandbox-runner:$VERSION\""*) echo "runner image $REG/nbsandbox-runner:$VERSION" ;;
	*) echo "GET /v1/runtime doesn't report the published runner image: ${runtime:0:300}" >&2; exit 1 ;; esac
id_of() { sed 's/.*"id":"\([^"]*\)".*/\1/'; }
space=$("$client" spaces create published-check | id_of)

# run NOTEBOOK: starts it, waits, fails unless it succeeded.
run() {
	local id result
	id=$("$client" runs start "$space" "$1" | id_of)
	result=$("$client" runs wait "$space" "$id")
	case "$result" in *'"status":"succeeded"'*) echo "$1: succeeded" ;;
		*) echo "$1: ${result:0:500}" >&2; return 1 ;; esac
}

step "The smoke notebook (deployment.md, step 8)"
"$client" files upload "$space" "$SRC/tests/workloads/smoke.ipynb" >/dev/null
run smoke.ipynb

step "A package set, built by the published builder image"
printf 'humanize==4.15.0\n' > "$WORK/requirements.txt"
cat > "$WORK/packages.ipynb" <<'EOF'
{"cells": [{"cell_type": "code", "execution_count": null, "metadata": {}, "outputs": [],
  "source": "import humanize\nassert humanize.__version__ == '4.15.0', humanize.__version__\nprint(humanize.intcomma(1234567))"}],
 "metadata": {"kernelspec": {"display_name": "Python 3", "language": "python", "name": "python3"}},
 "nbformat": 4, "nbformat_minor": 5}
EOF
"$client" files upload "$space" "$WORK/requirements.txt" "$WORK/packages.ipynb" >/dev/null
run packages.ipynb

# An interactive session (releases that have them): the release's own
# example opens one through Caddy, runs cells over its WebSocket, completes,
# interrupts, saves a file to the space and ends it.
if [ -f "$SRC/docs/sessions.md" ]; then
	step "An interactive session, with the release's example (examples/python/interactive_session.py)"
	python3 -m venv "$WORK/venv"
	# Without the local CA: pip's requests honours CURL_CA_BUNDLE too, and
	# would then trust only Caddy's local authority, not PyPI's certificate.
	env -u CURL_CA_BUNDLE "$WORK/venv/bin/pip" install -q requests websocket-client
	out=$(WEBSOCKET_CLIENT_CA_BUNDLE=$CURL_CA_BUNDLE "$WORK/venv/bin/python" "$SRC/examples/python/interactive_session.py" 2>&1) ||
		{ echo "$out" >&2; exit 1; }
	echo "$out"
	for want in "connected: kernel" "completes to: ['amount', 'month', 'region']" "interrupted the long cell" "totals.csv in the space: True" "session ended"; do
		case "$out" in *"$want"*) ;; *) echo "the session example didn't print: $want" >&2; exit 1 ;; esac
	done
	session=$(echo "$out" | sed -n 's/^session \([0-9a-f-]*\):.*/\1/p')
	[ -n "$session" ] && [ -z "$(docker ps -aq --filter "name=nbsandbox-session-$session")" ] ||
		{ echo "the session's container is left behind (or no session ID printed)" >&2; exit 1; }
fi

left=$(docker ps -aq --filter "name=nbsandbox-run-" | wc -l | tr -d ' ')
[ "$left" = 0 ] || { echo "$left run containers left behind" >&2; exit 1; }

step "v$VERSION as published: ok"
