#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Enforces the coverage thresholds:
#   core packages >= 80 %, whole module >= 70 %.
# Usage: scripts/coverage-gate.sh coverage.out
# The profile must come from `go test -coverprofile=coverage.out ./...`.

set -eu

profile="${1:-coverage.out}"
module=github.com/azbyluthfan/nbsandbox
core="internal/runs internal/logs internal/spaces internal/store internal/isolation/docker internal/auth internal/blob"
core_min=80
total_min=70

[ -f "$profile" ] || { echo "coverage profile $profile not found"; exit 1; }

fail=0
# Per-package coverage from the profile: statements covered / statements total.
for pkg in $core; do
	pct=$(awk -v p="$module/$pkg/" '
		NR > 1 && index($1, p) == 1 && substr($1, length(p) + 1) !~ /\// {
			split($0, f, " "); n = f[2]; c = f[3]
			total += n; if (c > 0) covered += n
		}
		END { if (total == 0) print "none"; else printf "%.1f", 100 * covered / total }' "$profile")
	if [ "$pct" = none ]; then
		printf '  %-28s no code yet, skipped\n' "$pkg"
	elif awk -v a="$pct" -v b="$core_min" 'BEGIN { exit !(a < b) }'; then
		printf '  %-28s %5s%%  FAIL (< %d%%)\n' "$pkg" "$pct" "$core_min"; fail=1
	else
		printf '  %-28s %5s%%  ok\n' "$pkg" "$pct"
	fi
done

# The total leaves out generated code (protobuf and Connect stubs).
handwritten=$(mktemp)
trap 'rm -f "$handwritten"' EXIT
grep -v '/gen/' "$profile" > "$handwritten"
total=$(go tool cover -func="$handwritten" | awk '/^total:/ { sub("%", "", $3); print $3 }')
if awk -v a="$total" -v b="$total_min" 'BEGIN { exit !(a < b) }'; then
	printf '  %-28s %5s%%  FAIL (< %d%%)\n' "total" "$total" "$total_min"; fail=1
else
	printf '  %-28s %5s%%  ok\n' "total" "$total"
fi

[ $fail -eq 0 ] || { echo "coverage gate: FAILED"; exit 1; }
echo "coverage gate: ok"
