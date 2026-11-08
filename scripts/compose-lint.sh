#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Checks the production Compose file keeps its hardening.
# Usage: scripts/compose-lint.sh [deploy/docker-compose.yml]

set -eu
FILE="${1:-deploy/docker-compose.yml}"

# Placeholder secrets: the file needs them to render, and they are never used.
DOCKER_GID=999 NBSANDBOX_DATA_DIR=/srv/nbsandbox NBSANDBOX_DB_PASSWORD=lint NBSANDBOX_S3_SECRET_KEY=lint docker compose -f "$FILE" --profile monitoring config --format json | python3 -c '
import json, sys
c = json.load(sys.stdin)
s, nets = c["services"], c.get("networks", {})
problems = []
def check(cond, msg):
    if not cond:
        problems.append(msg)

for name, svc in s.items():
    check(not svc.get("privileged"), f"{name}: privileged")
    if name != "prometheus":
        check(svc.get("read_only") is True, f"{name}: root filesystem not read-only")
        check(svc.get("cap_drop") == ["ALL"], f"{name}: capabilities not all dropped")
        check(any("no-new-privileges" in o for o in svc.get("security_opt", [])), f"{name}: no-new-privileges missing")
    for v in svc.get("volumes", []):
        if "docker.sock" in str(v.get("source", "")):
            check(name == "socket-proxy", f"{name}: mounts the Docker socket")
            check(v.get("read_only") is True, f"{name}: Docker socket mounted read-write")

n = s["nbsandboxd"]
check(n.get("user") == "1000:1000", "nbsandboxd: not running as uid 1000")
check(not n.get("ports"), "nbsandboxd: ports published (Caddy must be the only entry)")
check(str(n["environment"].get("NBSANDBOX_UI_ENABLED", "")).lower() == "false", "nbsandboxd: web UI not explicitly disabled")
check("healthcheck" in n, "nbsandboxd: no healthcheck")
check(set(s["socket-proxy"]["networks"]) == {"docker-api"}, "socket-proxy: on a network other than docker-api")
check(not s["socket-proxy"].get("ports"), "socket-proxy: ports published")
check(nets.get("docker-api", {}).get("internal") is True, "docker-api network is not internal")
check(s["caddy"].get("cap_add", []) == ["NET_BIND_SERVICE"], "caddy: unexpected capabilities")
for p in s.get("prometheus", {}).get("ports", []):
    check(p.get("host_ip") == "127.0.0.1", "prometheus: published beyond loopback")
for name, svc in s.items():
    img = svc.get("image", "")
    if name in ("caddy", "socket-proxy", "prometheus"):
        check("@sha256:" in img, f"{name}: image not pinned by digest")

if problems:
    print("compose lint: FAILED")
    for p in problems:
        print("  -", p)
    sys.exit(1)
print(f"compose lint: ok ({len(s)} services)")
'
