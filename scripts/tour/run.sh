#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Retakes the screenshots of docs/tour.md (make tour). Needs the dev stack
# (make up): gives alice (TOUR_USER) a "sales analysis" space with the example
# notebooks (an earlier tour's, its old runs removed, or a new one: space.py),
# then drives the web UI in a headless browser
# (Playwright, in a container on the stack's network). TOUR_ONLY=sessions
# retakes only the live session's screenshots (13-15); it needs one free
# session for the user (sessions.per_user). TOUR_ONLY=compute retakes only
# 16-20 (connections, the compute view, members). The connections steps need them on:
# NBSANDBOX_CONNECTIONS_ENABLED=true NBSANDBOX_CONNECTIONS_ALLOWED=crm.db.internal,api.internal,idp.internal make up
set -eu
cd "$(dirname "$0")/../.."
API=${NBSANDBOX_URL:-http://127.0.0.1:8080}
TOUR_USER=${TOUR_USER:-alice}
AUTH=$TOUR_USER:$TOUR_USER-dev-password
OUT=docs/images/tour
PLAYWRIGHT=mcr.microsoft.com/playwright/python:v1.49.0-noble

space=$(python3 scripts/tour/space.py "$API" "$AUTH" "sales analysis") ||
	{ echo "could not get a space (is make up running?)" >&2; exit 1; }
curl -fsS -u "$AUTH" -F file=@examples/notebooks/sales_report.ipynb -F file=@examples/notebooks/sales_by_month.ipynb \
	"$API/v1/spaces/$space/files" >/dev/null
curl -fsS -u "$AUTH" -F file=@examples/notebooks/data/sales.csv "$API/v1/spaces/$space/files?folder=data" >/dev/null
curl -fsS -u "$AUTH" -F file=@examples/notebooks/compute_demo.ipynb "$API/v1/spaces/$space/files" >/dev/null
echo "space $space"
# Shared with bob, for the Members screenshot (20).
curl -fsS -u "$AUTH" -X PUT -H 'Content-Type: application/json' -d '{"role":"write"}' "$API/v1/spaces/$space/members/bob" >/dev/null

# With connections on: a database named crm in two environments, and an
# HTTP API named events, in a space of its own (a space that binds names
# starts a fetcher for each run).
conn_space=""
# An earlier tour's connection is used again.
conn=$(curl -fsS -u "$AUTH" "$API/v1/connections" 2>/dev/null | tr '{' '\n' | grep '"name":"crm-prod"' | sed -n 's/.*"id":"\([0-9a-f-]*\)".*/\1/p' | head -n 1) || true
if [ -z "$conn" ]; then
	conn=$(curl -fsS -u "$AUTH" -H 'Content-Type: application/json' \
		-d '{"name":"crm-prod","type":"postgresql","settings":{"host":"crm.db.internal","database":"crm","user":"nbsandbox_reader"},"secret":{"value":"tour-only"}}' \
		"$API/v1/connections" 2>/dev/null | sed -n 's/^{"id":"\([0-9a-f-]*\)".*/\1/p') || true
fi
api=$(curl -fsS -u "$AUTH" "$API/v1/connections" 2>/dev/null | tr '{' '\n' | grep '"name":"events-api"' | sed -n 's/.*"id":"\([0-9a-f-]*\)".*/\1/p' | head -n 1) || true
if [ -n "$conn" ] && [ -z "$api" ]; then
	api=$(curl -fsS -u "$AUTH" -H 'Content-Type: application/json' \
		-d '{"name":"events-api","type":"http","settings":{"url":"https://api.internal/v1/","auth":"oauth2","client_id":"nbsandbox","token_url":"https://idp.internal/oauth/token"},"secret":{"value":"tour-only"}}' \
		"$API/v1/connections" 2>/dev/null | sed -n 's/^{"id":"\([0-9a-f-]*\)".*/\1/p') || true
fi
if [ -n "$conn" ]; then
	conn_space=$(python3 scripts/tour/space.py "$API" "$AUTH" "crm reports")
	events=""
	[ -n "$api" ] && events=",\"events\":\"$api\""
	curl -fsS -u "$AUTH" -X PUT -H 'Content-Type: application/json' \
		-d "{\"default_environment\":\"dev\",\"environments\":{\"dev\":{\"crm\":\"$conn\"$events},\"prod\":{\"crm\":\"$conn\"$events}}}" \
		"$API/v1/spaces/$conn_space/connections" >/dev/null
	echo "connection $conn"
else
	echo "connections are off: the tour skips their screenshots (16-17)"
fi

mkdir -p "$OUT"
rm -f "$OUT/failed.png"
docker run --rm --network nbsandbox-dev_default -v "$PWD/scripts/tour":/tour:ro -v "$PWD/$OUT":/out \
	-e TOUR_USER="$TOUR_USER" -e TOUR_ONLY="${TOUR_ONLY:-}" -e TOUR_CONN_SPACE="$conn_space" "$PLAYWRIGHT" \
	sh -c "pip install -q --root-user-action=ignore playwright==1.49.0 2>/dev/null; python /tour/tour.py http://nbsandboxd:8090 $space /out"
