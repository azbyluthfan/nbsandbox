#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Fails if a Go, Python or shell source file lacks the SPDX licence line
# within its first 5 lines. Usage: scripts/check-license-headers.sh [dir]

set -eu

root="${1:-.}"
# Files whose first 5 lines lack the SPDX identifier (handles any file name).
missing_files=$(find "$root" -type f \( -name '*.go' -o -name '*.py' -o -name '*.sh' \) \
	-not -path '*/.git/*' -not -path '*/bin/*' -not -path '*/dist/*' -not -path '*/vendor/*' \
	-exec sh -c 'head -n 5 "$1" | grep -q "SPDX-License-Identifier: Apache-2.0" || echo "missing licence header: $1"' _ {} \;)
missing=0
if [ -n "$missing_files" ]; then
	echo "$missing_files"
	missing=1
fi

if [ "$missing" -ne 0 ]; then
	echo
	echo "Add these lines at the top of each file (comment syntax for the language):"
	echo "  Copyright 2026 The nbsandbox Authors"
	echo "  SPDX-License-Identifier: Apache-2.0"
	exit 1
fi
echo "licence headers: ok"
