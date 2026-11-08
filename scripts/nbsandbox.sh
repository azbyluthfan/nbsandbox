#!/usr/bin/env bash
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Example nbsandbox client (curl).
#
#   NBSANDBOX_URL       default http://127.0.0.1:8080
#   NBSANDBOX_USER      default alice
#   NBSANDBOX_PASSWORD  required (dev stack: alice-dev-password)
#
# Usage:
#   nbsandbox.sh spaces list | create [name] | get ID | rename ID NAME | delete ID
#   nbsandbox.sh files  list ID | upload ID FILE... [--folder FOLDER] | get ID PATH [OUT] | delete ID PATH
#                     put ID PATH LOCALFILE [VERSION|new] | move ID PATH NEWPATH | new-folder ID PATH | delete-folder ID PATH
#   nbsandbox.sh runs   start ID NOTEBOOK [PARAMS_JSON] | list ID | get ID RUN | wait ID RUN
#                     log ID RUN | cancel ID RUN | artifacts ID RUN | artifact ID RUN PATH [OUT]
#                     notebook ID RUN [OUT] | delete ID RUN | follow ID RUN [AFTER]
#                     result ID RUN [WAIT_SECONDS] | events ID RUN
#   nbsandbox.sh functions list ID | get ID NAME | publish ID NAME NOTEBOOK [FILE...] | contract ID NAME
#                     invoke ID NAME [PARAMS_JSON] [WAIT_SECONDS] | delete ID NAME
#   nbsandbox.sh ready | runtime

set -euo pipefail

URL="${NBSANDBOX_URL:-http://127.0.0.1:8080}"
USER_NAME="${NBSANDBOX_USER:-alice}"

die() { echo "nbsandbox.sh: $*" >&2; exit 1; }

api() { # METHOD PATH [curl args...]; prints the body, fails on HTTP >= 400
	local method=$1 path=$2; shift 2
	[ -n "${NBSANDBOX_PASSWORD:-}" ] || die "set NBSANDBOX_PASSWORD"
	local out code
	out=$(mktemp)
	# Credentials go through a config on stdin so they don't show in `ps`.
	code=$(printf 'user = "%s:%s"\n' "$USER_NAME" "$NBSANDBOX_PASSWORD" |
		curl -sS -K - -o "$out" -w '%{http_code}' -X "$method" "$@" "$URL$path") || { rm -f "$out"; die "request failed"; }
	cat "$out"; [ -s "$out" ] && [ "$(tail -c1 "$out")" != "" ] && echo
	rm -f "$out"
	[ "$code" -lt 400 ] || { echo "HTTP $code" >&2; return 1; }
}

urlpath() { # encode each path segment, keep the slashes
	local IFS=/ seg parts=()
	for seg in $1; do parts+=("$(printf '%s' "$seg" | jq -sRr @uri 2>/dev/null || printf '%s' "$seg")"); done
	(IFS=/; echo "${parts[*]}")
}

cmd=${1:-}; sub=${2:-}
case "$cmd $sub" in
	"ready "*)          curl -sS "$URL/readyz"; echo ;;
	"spaces list")      api GET /v1/spaces ;;
	"spaces create")    api POST /v1/spaces -H 'Content-Type: application/json' --data "{\"name\": \"${3:-}\"}" ;;
	"spaces get")       api GET "/v1/spaces/${3:?space id}" ;;
	"spaces rename")    api PATCH "/v1/spaces/${3:?space id}" -H 'Content-Type: application/json' --data "{\"name\": \"${4:?name}\"}" ;;
	"spaces delete")    api DELETE "/v1/spaces/${3:?space id}" && echo "deleted" ;;
	"files list")       api GET "/v1/spaces/${3:?space id}/files" ;;
	"files upload")
		id=${3:?space id}; shift 3
		folder=""; args=()
		while [ $# -gt 0 ]; do
			case "$1" in
				--folder) folder=${2:?folder}; shift 2 ;;
				*) [ -f "$1" ] || die "no such file: $1"; args+=(-F "file=@$1"); shift ;;
			esac
		done
		[ ${#args[@]} -gt 0 ] || die "nothing to upload"
		q=""; [ -n "$folder" ] && q="?folder=$(printf '%s' "$folder" | jq -sRr @uri 2>/dev/null || echo "$folder")"
		api POST "/v1/spaces/$id/files$q" "${args[@]}" ;;
	"files get")
		p=${4:?path}; out=${5:-$(basename "$p")}
		api GET "/v1/spaces/${3:?space id}/files/$(urlpath "$p")" > "$out" && echo "saved $out" ;;
	"files delete")     api DELETE "/v1/spaces/${3:?space id}/files/$(urlpath "${4:?path}")" && echo "deleted" ;;
	"runtime "*)        api GET /v1/runtime ;;
	"runs start")
		params=${5:-}
		body="{\"notebook\": \"${4:?notebook}\"${params:+, \"parameters\": $params}}"
		api POST "/v1/spaces/${3:?space id}/runs" -H 'Content-Type: application/json' --data "$body" ;;
	"runs list")        api GET "/v1/spaces/${3:?space id}/runs" ;;
	"runs get")         api GET "/v1/spaces/${3:?space id}/runs/${4:?run id}" ;;
	"runs wait")
		while :; do
			out=$(api GET "/v1/spaces/${3:?space id}/runs/${4:?run id}")
			case "$out" in *'"status":"queued"'*|*'"status":"running"'*) sleep 1 ;; *) echo "$out"; break ;; esac
		done ;;
	"runs log")         api GET "/v1/spaces/${3:?space id}/runs/${4:?run id}/log" ;;
	"runs cancel")      api POST "/v1/spaces/${3:?space id}/runs/${4:?run id}/cancel" ;;
	"runs artifacts")   api GET "/v1/spaces/${3:?space id}/runs/${4:?run id}/artifacts" ;;
	"runs artifact")
		p=${5:?path}; out=${6:-$(basename "$p")}
		api GET "/v1/spaces/${3:?space id}/runs/${4:?run id}/artifacts/$(urlpath "$p")" > "$out" && echo "saved $out" ;;
	"runs notebook")
		out=${5:-output.ipynb}
		api GET "/v1/spaces/${3:?space id}/runs/${4:?run id}/output.ipynb" > "$out" && echo "saved $out" ;;
	"runs follow")
		# Live log (Server-Sent Events): print each line as it arrives, stop at
		# "done". Resume after a line number with AFTER.
		[ -n "${NBSANDBOX_PASSWORD:-}" ] || die "set NBSANDBOX_PASSWORD"
		printf 'user = "%s:%s"\n' "$USER_NAME" "$NBSANDBOX_PASSWORD" |
			curl -sS -N -K - -H "Last-Event-ID: ${5:-0}" "$URL/v1/spaces/${3:?space id}/runs/${4:?run id}/log/stream" |
			awk '/^event: /{ev=substr($0,8)} /^data: /{d=substr($0,7); if (ev=="line") { print d; fflush() } else { print "[" ev "] " d; exit } }' ;;
	"runs delete")      api DELETE "/v1/spaces/${3:?space id}/runs/${4:?run id}" && echo "deleted" ;;
	"runs result")      api GET "/v1/spaces/${3:?space id}/runs/${4:?run id}/result?wait=${5:-0}" ;;
	"runs events")
		# Status changes as they happen (Server-Sent Events); ends after the
		# final status, which carries the result.
		[ -n "${NBSANDBOX_PASSWORD:-}" ] || die "set NBSANDBOX_PASSWORD"
		printf 'user = "%s:%s"\n' "$USER_NAME" "$NBSANDBOX_PASSWORD" |
			curl -sS -N -K - "$URL/v1/spaces/${3:?space id}/runs/${4:?run id}/events" |
			awk '/^data: /{print substr($0,7); fflush()}' ;;
	"files put")
		# Save a file; VERSION is the one you are replacing (from "files list"), or "new".
		f=${5:?local file}; v=${6:-new}
		[ -f "$f" ] || die "no such file: $f"
		if [ "$v" = new ]; then h='If-None-Match: *'; else h="If-Match: \"$v\""; fi
		api PUT "/v1/spaces/${3:?space id}/files/$(urlpath "${4:?path}")" -H "$h" --data-binary "@$f" ;;
	"files move")       api PATCH "/v1/spaces/${3:?space id}/files/$(urlpath "${4:?path}")" -H 'Content-Type: application/json' --data "{\"path\": \"${5:?new path}\"}" ;;
	"files new-folder") api POST "/v1/spaces/${3:?space id}/folders" -H 'Content-Type: application/json' --data "{\"path\": \"${4:?path}\"}" ;;
	"files delete-folder") api DELETE "/v1/spaces/${3:?space id}/files/$(urlpath "${4:?path}")?recursive=true" && echo "deleted" ;;
	"functions list")   api GET "/v1/spaces/${3:?space id}/functions" ;;
	"functions get")    api GET "/v1/spaces/${3:?space id}/functions/${4:?name}" ;;
	"functions publish")
		# NOTEBOOK and the FILES/FOLDERS it needs, as space paths.
		id=${3:?space id}; name=${4:?name}; nbp=${5:?notebook}; shift 5
		files=$(printf '%s\n' "$@" | jq -R . | jq -sc .)
		api POST "/v1/spaces/$id/functions/$name/versions" -H 'Content-Type: application/json' \
			--data "{\"notebook\": \"$nbp\", \"files\": $files}" ;;
	"functions invoke")
		# Waits up to WAIT seconds (default 30) for the result.
		api POST "/v1/spaces/${3:?space id}/functions/${4:?name}/invoke?wait=${6:-30}" -H 'Content-Type: application/json' \
			--data "{\"parameters\": ${5:-{\}}}" ;;
	"functions contract") api GET "/v1/spaces/${3:?space id}/functions/${4:?name}/contract" ;;
	"functions delete") api DELETE "/v1/spaces/${3:?space id}/functions/${4:?name}" && echo "deleted" ;;
	*) sed -n '5,/^$/p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
