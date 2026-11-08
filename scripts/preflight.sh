#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Checks a host's prerequisites for nbsandbox. Detects Linux vs
# Docker Desktop and adapts the ownership checks.
#
# Environment (same names as nbsandboxd):
#   NBSANDBOX_DATA_DIR        default /srv/nbsandbox
#   NBSANDBOX_DOCKER_RUNTIME  "" or runsc
#   NBSANDBOX_RUNNER_IMAGE    default nbsandbox-runner:dev
#   NBSANDBOX_USERS_FILE      optional; checked if set

set -u

DATA_DIR="${NBSANDBOX_DATA_DIR:-/srv/nbsandbox}"
RUNTIME="${NBSANDBOX_DOCKER_RUNTIME:-}"
IMAGE="${NBSANDBOX_RUNNER_IMAGE:-nbsandbox-runner:dev}"
USERS_FILE="${NBSANDBOX_USERS_FILE:-}"
MIN_FREE_MB=1024

fail=0
ok()   { printf '  ok    %s\n' "$*"; }
warn() { printf '  warn  %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fail=1; }

echo "nbsandbox preflight"

# Docker
if ! server=$(docker version --format '{{.Server.Version}}' 2>/dev/null); then
	bad "Docker is not reachable (is Docker running? DOCKER_HOST=${DOCKER_HOST:-unset})"
	echo; echo "preflight: FAILED"; exit 1
fi
os_name=$(docker info --format '{{.OperatingSystem}}' 2>/dev/null)
cgroup=$(docker info --format '{{.CgroupVersion}}' 2>/dev/null)
runtimes=$(docker info --format '{{range $k, $v := .Runtimes}}{{$k}} {{end}}' 2>/dev/null)
case "$os_name" in
	*"Docker Desktop"*) platform=desktop ;;
	*) platform=linux ;;
esac
ok "Docker $server ($os_name)"
if [ "$platform" = desktop ]; then
	warn "Docker Desktop: supported for development; isolation results are non-reference"
fi
[ "$cgroup" = 2 ] && ok "cgroup v2" || bad "cgroup v$cgroup: nbsandbox needs cgroup v2 for OOM and usage evidence"

if [ -n "$RUNTIME" ]; then
	case " $runtimes " in
		*" $RUNTIME "*) ok "runtime $RUNTIME registered" ;;
		*) bad "runtime $RUNTIME is configured but Docker only has: $runtimes" ;;
	esac
fi

if docker image inspect "$IMAGE" >/dev/null 2>&1; then
	ok "runner image $IMAGE present"
else
	bad "runner image $IMAGE missing (make runner-image)"
fi

# Data directory
if [ ! -d "$DATA_DIR" ]; then
	bad "data dir $DATA_DIR does not exist"
else
	mode=$(stat -c '%a' "$DATA_DIR" 2>/dev/null || stat -f '%Lp' "$DATA_DIR")
	owner=$(stat -c '%u' "$DATA_DIR" 2>/dev/null || stat -f '%u' "$DATA_DIR")
	if [ "$platform" = linux ]; then
		[ "$owner" = 1000 ] && ok "data dir owned by uid 1000" || bad "data dir $DATA_DIR is owned by uid $owner; nbsandboxd and runs use uid 1000 (chown -R 1000:1000 $DATA_DIR)"
		[ "$mode" = 700 ] && ok "data dir mode 700" || bad "data dir mode is $mode; want 700 (chmod 700 $DATA_DIR)"
	else
		ok "data dir $DATA_DIR exists (ownership checks skipped on Docker Desktop)"
		case "$DATA_DIR" in
			/Users/*|/Volumes/*|/private/*|/tmp/*) ;;
			*) warn "data dir is outside Docker Desktop's default file sharing (/Users, /Volumes, /private, /tmp)" ;;
		esac
	fi
	free_mb=$(df -Pm "$DATA_DIR" | awk 'NR==2 {print $4}')
	[ "${free_mb:-0}" -ge $MIN_FREE_MB ] && ok "free disk ${free_mb} MiB" || bad "only ${free_mb} MiB free in $DATA_DIR (want at least $MIN_FREE_MB)"
fi

# Users file
if [ -n "$USERS_FILE" ]; then
	if [ ! -f "$USERS_FILE" ]; then
		bad "users file $USERS_FILE does not exist"
	else
		umode=$(stat -c '%a' "$USERS_FILE" 2>/dev/null || stat -f '%Lp' "$USERS_FILE")
		case "$umode" in
			*0) ok "users file mode $umode" ;;
			*) bad "users file mode $umode lets other users read it (chmod 600 $USERS_FILE)" ;;
		esac
	fi
fi

echo
if [ $fail -ne 0 ]; then echo "preflight: FAILED"; exit 1; fi
echo "preflight: ok"
