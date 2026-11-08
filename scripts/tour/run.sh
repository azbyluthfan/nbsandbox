#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Retakes the screenshots of docs/tour.md (make tour). Needs the dev stack
# (make up): creates a fresh "sales analysis" space for alice with the
# example notebooks, then drives the web UI in a headless browser
# (Playwright, in a container on the stack's network).
set -eu
cd "$(dirname "$0")/../.."
API=${NBSANDBOX_URL:-http://127.0.0.1:8080}
AUTH=alice:alice-dev-password
OUT=docs/images/tour
PLAYWRIGHT=mcr.microsoft.com/playwright/python:v1.49.0-noble

space=$(curl -fsS -u "$AUTH" -H 'Content-Type: application/json' -d '{"name":"sales analysis"}' "$API/v1/spaces" |
	sed -n 's/.*"id":"\([0-9a-f-]*\)".*/\1/p')
[ -n "$space" ] || { echo "could not create a space (is make up running?)" >&2; exit 1; }
curl -fsS -u "$AUTH" -F file=@examples/notebooks/sales_report.ipynb -F file=@examples/notebooks/sales_by_month.ipynb \
	"$API/v1/spaces/$space/files" >/dev/null
curl -fsS -u "$AUTH" -F file=@examples/notebooks/data/sales.csv "$API/v1/spaces/$space/files?folder=data" >/dev/null
echo "space $space"

mkdir -p "$OUT"
rm -f "$OUT/failed.png"
docker run --rm --network nbsandbox-dev_default -v "$PWD/scripts/tour":/tour:ro -v "$PWD/$OUT":/out "$PLAYWRIGHT" \
	sh -c "pip install -q --root-user-action=ignore playwright==1.49.0 2>/dev/null; python /tour/tour.py http://nbsandboxd:8090 $space /out"
