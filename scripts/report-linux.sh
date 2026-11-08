#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# The reference isolation report, from a Mac: runs the integration suite on
# Linux with Docker Engine in a Lima VM, once per runtime (runc, and gVisor's
# runsc), and writes docs/isolation-report-linux.md. make report-linux.
#
# The VM needs Docker Engine, and for runsc gVisor registered as the Docker
# runtime "runsc" (docs/gvisor.md). It builds nothing: the images are built
# here and copied in, and the suite is compiled here for Linux.
#
# Environment: LIMA_VM (default gvisor), RUNTIMES (default "runc runsc"),
# RUN_MEMORY (memory per run, default 512MiB: the reference VM has 2 GB),
# REPORT_OUT (default docs/isolation-report-linux.md), TESTFLAGS.
set -eu
cd "$(dirname "$0")/.."
REPO=$(pwd)
VM=${LIMA_VM:-gvisor}
RUNTIMES=${RUNTIMES:-runc runsc}
MEMORY=${RUN_MEMORY:-512MiB}
STATE=/var/lib/nbsandbox-report
OUT=bin/findings-linux
REPORT=${REPORT_OUT:-docs/isolation-report-linux.md}
GO=${GO:-go}
vm() { limactl shell "$VM" -- sudo "$@"; }

arch=$(vm uname -m)
case $arch in aarch64 | arm64) goarch=arm64 ;; x86_64) goarch=amd64 ;; *) echo "unknown VM architecture $arch" >&2; exit 1 ;; esac

echo "building images and the suite for linux/$goarch"
make runner-image
docker build -q -f deploy/nbsandboxd.Dockerfile -t nbsandboxd:dev . >/dev/null
mkdir -p bin/linux
GOOS=linux GOARCH=$goarch CGO_ENABLED=0 $GO test -c -tags integration -o bin/linux/integration.test ./tests/integration/

echo "copying them into $VM"
docker save nbsandboxd:dev nbsandbox-runner:dev | vm docker load
vm mkdir -p "$STATE"
vm install -m 755 "$REPO/bin/linux/integration.test" "$STATE/integration.test"

for rt in $RUNTIMES; do
	echo "suite with $rt (log: $STATE/$rt.log in $VM)"
	vm env TESTFLAGS="${TESTFLAGS:-}" sh "$REPO/scripts/report-linux-vm.sh" "$REPO" "$STATE" "$rt" "$MEMORY" >/dev/null || true
done

rm -rf "$OUT" && mkdir -p "$OUT"
vm tar -C "$STATE/findings" -cf - . | tar -xf - -C "$OUT"
$GO run ./cmd/nbsandbox-report -in "$OUT" -out "$REPORT" -target "make report-linux" -require
