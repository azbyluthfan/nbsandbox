#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Runs the integration suite for one Docker runtime on a Linux host with
# Docker Engine (run as root). make report-linux calls it inside a Lima VM;
# on a Linux server, run it there the same way.
#
#   report-linux-vm.sh <repo> <state dir> <runtime: runc|runsc> <memory per run>
#
# The repository may be read-only (Lima mounts the Mac's home that way);
# everything written goes to the state dir: the data folder, the settings
# for this runtime, the suite's log and the findings (<state>/findings).
set -eu
REPO=$1 STATE=$2 RT=$3 MEMORY=$4

mkdir -p "$STATE/data/spaces" "$STATE/findings"
install -m 600 "$REPO/config/users.yaml" "$STATE/data/users.yaml"
chown -R 1000:1000 "$STATE/data" && chmod 700 "$STATE/data"

runtime=""
[ "$RT" = runsc ] && runtime=runsc
sed -e "s/^  memory: [^ ]*/  memory: $MEMORY/" -e "s/^  runtime: \"\"/  runtime: \"$runtime\"/" \
	"$REPO/config/settings.yaml" > "$STATE/settings-$RT.yaml"

DOCKER_GID=$(getent group docker | cut -d: -f3)
export NBSANDBOX_DATA_DIR="$STATE/data" NBSANDBOX_SETTINGS="$STATE/settings-$RT.yaml" DOCKER_GID \
	NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_RUN_TIMEOUT=60s REPORT_DIR="$STATE/findings" HOME=/root \
	NBSANDBOX_CPU_FAIRNESS_GATE=1
rm -rf "$STATE/findings/$RT"
docker compose -f "$REPO/deploy/docker-compose.dev.yml" up -d --wait
cd "$REPO/tests/integration"
"$STATE/integration.test" -test.v -test.count=1 -test.timeout 90m ${TESTFLAGS:-} 2>&1 | tee "$STATE/$RT.log"
