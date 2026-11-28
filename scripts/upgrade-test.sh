#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# The upgrade test (make test-upgrade): the dev stack of an earlier release
# (FROM, a tag, with its published nbsandboxd image; the Makefile names the
# newest release) fills a database
# and object store; this checkout's nbsandboxd then starts on the same
# volumes, and TestUpgrade checks that everything made before is still there
# and still works. It uses its own Compose project (nbsandbox-upgrade) and
# data folder, so the dev stack's data is left alone; the dev stack itself
# is stopped (it publishes the same ports). Everything is removed at the end.
# DATA_OWNER=1000:1000 hands the data folder to nbsandboxd's user with sudo
# (Linux with Docker Engine, where the folder isn't otherwise writable to it).
set -eu

FROM=${FROM:?set FROM to an earlier release tag, e.g. v0.3.0}; export FROM
FROM_IMAGE=${FROM_IMAGE:-ghcr.io/azbyluthfan/nbsandboxd:${FROM#v}}
GO=${GO:-go}
root=$(cd "$(dirname "$0")/.." && pwd)
data=${NBSANDBOX_DATA_DIR:-$HOME/.local/share/nbsandbox}-upgrade
work=$(mktemp -d "${TMPDIR:-/tmp}/nbsandbox-upgrade.XXXXXX")
old="$work/$FROM"
project=nbsandbox-upgrade
export NBSANDBOX_DATA_DIR="$data" NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_SESSIONS_PER_USER=0

up_old() { docker compose -p "$project" -f "$old/deploy/docker-compose.dev.yml" -f "$work/from-image.yml" "$@"; }
up_new() { docker compose -p "$project" -f "$root/deploy/docker-compose.dev.yml" "$@"; }

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		echo "upgrade test failed; nbsandboxd's log:" >&2
		up_new logs --no-color --tail 80 nbsandboxd >&2 2>/dev/null || up_old logs --no-color --tail 80 nbsandboxd >&2 2>/dev/null || true
	fi
	up_new down -v --remove-orphans >/dev/null 2>&1 || true
	git -C "$root" worktree remove --force "$old" >/dev/null 2>&1 || true
	rm -rf "$work"
	if [ -n "${DATA_OWNER:-}" ]; then sudo rm -rf "$data"; else rm -rf "$data"; fi 2>/dev/null || true
	exit "$status"
}
trap cleanup EXIT INT TERM

"$root/scripts/stop-kind.sh"
docker compose -f "$root/deploy/docker-compose.dev.yml" down >/dev/null 2>&1 || true
docker compose -f "$root/deploy/docker-compose.cluster.yml" stop >/dev/null 2>&1 || true
up_new down -v --remove-orphans >/dev/null 2>&1 || true

mkdir -p "$data/spaces" && chmod 700 "$data"
install -m 600 "$root/config/users.yaml" "$data/users.yaml"
[ -z "${DATA_OWNER:-}" ] || sudo chown -R "$DATA_OWNER" "$data"
# CI checks out one commit without tags: fetch the one this needs.
git -C "$root" rev-parse -q --verify "refs/tags/${FROM}" >/dev/null ||
	git -C "$root" fetch -q --depth=1 origin "refs/tags/${FROM}:refs/tags/${FROM}"
git -C "$root" worktree add --detach "$old" "$FROM" >/dev/null
cat >"$work/from-image.yml" <<EOF
services:
  nbsandboxd:
    image: $FROM_IMAGE
EOF
docker pull -q "$FROM_IMAGE" >/dev/null

echo "== $FROM ($FROM_IMAGE): make the data"
# Connections on (from 0.4.0; earlier releases ignore the settings), so
# names bound before the upgrade are upgraded too.
NBSANDBOX_CONNECTIONS_ENABLED=true NBSANDBOX_CONNECTIONS_ALLOWED=db.example.com \
	up_old up -d --no-build --wait
NBSANDBOX_UPGRADE=before NBSANDBOX_UPGRADE_STATE="$work/state.json" \
	"$GO" test -C "$root" -tags integration -count=1 -timeout 10m -run '^TestUpgrade$' ${TESTFLAGS:-} ./tests/integration/

echo "== this checkout: upgrade on the same volumes"
up_old stop nbsandboxd
NBSANDBOX_CONNECTIONS_ENABLED=true NBSANDBOX_CONNECTIONS_ALLOWED=db.example.com \
	up_new up -d --build --remove-orphans --wait
NBSANDBOX_UPGRADE=after NBSANDBOX_UPGRADE_STATE="$work/state.json" \
	"$GO" test -C "$root" -tags integration -count=1 -timeout 15m -run '^TestUpgrade$' ${TESTFLAGS:-} ./tests/integration/
