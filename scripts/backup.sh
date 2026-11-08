#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Backs up a running nbsandbox that uses the bundled PostgreSQL and RustFS
# (deploy/docker-compose.yml). Restore: docs/operations.md#restore.
#
#   1. the database (pg_dump: a consistent snapshot while nbsandboxd runs);
#   2. the object store's volume: every file's contents, run outputs and
#      published function versions (each stored once, never changed).
#
# The host data folder is not backed up: it holds only the working copies of
# runs that are active right now.
#
#   COMPOSE         compose command for the stack
#                   (default: docker compose -f deploy/docker-compose.yml --env-file deploy/.env)
#   OBJECTS_VOLUME  the object store's volume (default: nbsandbox_objects)
#
# With your own S3 store or Azure Blob Storage, back the database up with this
# script (or your PostgreSQL service's tools) and the bucket with the
# provider's versioning or replication.
#
# Usage: scripts/backup.sh DEST_DIR

set -eu

DEST="${1:?usage: scripts/backup.sh DEST_DIR}"
COMPOSE="${COMPOSE:-docker compose -f deploy/docker-compose.yml --env-file deploy/.env}"
OBJECTS_VOLUME="${OBJECTS_VOLUME:-nbsandbox_objects}"
# Any image with tar will do: this is the stack's own PostgreSQL image.
TAR_IMAGE=postgres:17-alpine@sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac
STAMP=$(date -u +%Y%m%dT%H%M%SZ)

mkdir -p "$DEST"

# 1. The database first. Contents are never changed once stored, so what this
#    snapshot refers to is still in the object store when step 2 runs, unless
#    a file is deleted in between and the hourly cleanup removes its contents
#    in that same moment (rare; then the restored file is missing).
$COMPOSE exec -T postgres pg_dump -U nbsandbox -d nbsandbox -Fc > "$DEST/nbsandbox-$STAMP.dump"

# 2. The object store's files, read-only, while it keeps running. Contents
#    stored after step 1 come along too; nothing refers to them after a
#    restore, and cleanup removes them.
docker run --rm -v "$OBJECTS_VOLUME":/data:ro --entrypoint tar "$TAR_IMAGE" -C /data -czf - . > "$DEST/objects-$STAMP.tar.gz"

echo "backup written:"
ls -l "$DEST/nbsandbox-$STAMP.dump" "$DEST/objects-$STAMP.tar.gz"
