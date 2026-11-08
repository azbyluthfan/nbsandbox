#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Checks a built runner image.
# Usage: runner-image/tests/image_test.sh nbsandbox-runner:dev

set -eu
IMAGE="${1:?image}"
fail=0
check() { if eval "$2"; then echo "  ok    $1"; else echo "  FAIL  $1"; fail=1; fi; }
inside() { docker run --rm --network none --entrypoint sh "$IMAGE" -c "$1"; }

echo "runner image tests: $IMAGE"
check "runs as uid 1000"            '[ "$(docker image inspect "$IMAGE" --format "{{.Config.User}}")" = "1000:1000" ]'
check "entrypoint is the wrapper"   'docker image inspect "$IMAGE" --format "{{json .Config.Entrypoint}}" | grep -q nbsandbox_runner.py'
check "HOME is /tmp"                'docker image inspect "$IMAGE" --format "{{json .Config.Env}}" | grep -q "HOME=/tmp"'
check "no setuid/setgid binaries"   '[ -z "$(inside "find / -xdev -type f -perm /6000 2>/dev/null")" ]'
check "python3 kernel installed"    'inside "jupyter kernelspec list" | grep -q python3'
check "core packages import"        'inside "python -c \"import papermill, nbconvert, ipykernel, numpy, pandas, matplotlib\""'

# shellcheck disable=SC2034 # used inside the eval'ed checks below
manifest=$(docker image inspect "$IMAGE" --format '{{index .Config.Labels "io.nbsandbox.manifest"}}')
check "manifest label present"      '[ -n "$manifest" ]'
check "manifest is valid JSON"      'printf "%s" "$manifest" | python3 -c "import json,sys; m=json.load(sys.stdin); assert m[\"status_schema_version\"]==2 and \"pandas\" in m[\"distributions\"] and m[\"import_names\"][\"sklearn\" if \"sklearn\" in m[\"import_names\"] else \"pandas\"]"'
check "manifest maps imports"       'printf "%s" "$manifest" | python3 -c "import json,sys; m=json.load(sys.stdin); assert m[\"import_names\"][\"yaml\"]==[\"pyyaml\"], m[\"import_names\"].get(\"yaml\")"'
check "wrapper unit tests pass"     'docker run --rm --network none -v "$(cd "$(dirname "$0")/.." && pwd)":/src:ro --entrypoint python "$IMAGE" -B -m unittest discover -s /src/tests -q >/dev/null 2>&1'

[ $fail -eq 0 ] && echo "runner image tests: ok" || { echo "runner image tests: FAILED"; exit 1; }
