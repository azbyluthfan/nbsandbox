#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Backs up a running nbsandbox: the database (pg_dump, consistent while the
# service runs) and an archive of the spaces. Restore: docs/operations.md#restore.
#
#   NBSANDBOX_DATA_DIR  data directory (default /srv/nbsandbox)
#   COMPOSE             compose command for the stack
#                       (default: docker compose -f deploy/docker-compose.yml --env-file deploy/.env)
#
# Usage: scripts/backup.sh DEST_DIR

set -eu

DEST="${1:?usage: scripts/backup.sh DEST_DIR}"
DATA="${NBSANDBOX_DATA_DIR:-/srv/nbsandbox}"
COMPOSE="${COMPOSE:-docker compose -f deploy/docker-compose.yml --env-file deploy/.env}"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)

mkdir -p "$DEST"
# 1. The database: pg_dump inside the postgres container takes a consistent
#    snapshot while nbsandboxd keeps running (custom format, for pg_restore).
$COMPOSE exec -T postgres pg_dump -U nbsandbox -d nbsandbox -Fc > "$DEST/nbsandbox-$STAMP.dump"

# 2. The spaces. Files are copied as they are at this moment; a run that is
#    writing at the same time may be caught half-way (its own files only).
#    For a point-in-time copy, use filesystem snapshots or stop the stack.
tar -C "$DATA" -czf "$DEST/spaces-$STAMP.tar.gz" spaces

echo "backup written:"
ls -l "$DEST/nbsandbox-$STAMP.dump" "$DEST/spaces-$STAMP.tar.gz"
