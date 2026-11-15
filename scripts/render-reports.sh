#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Renders the Linux and Kubernetes reference reports from a weekly CI run's
# artifacts, for a person to review and commit (usually in a release PR):
#
#   scripts/render-reports.sh <folder with the run's artifacts, unzipped>
#
# Download the run's isolation-report-docker-* and
# isolation-report-kubernetes-1.37-* artifacts from its page in the Actions
# tab and unzip each into a folder of its own name, or:
#   gh run download <run id> -p 'isolation-report-*' -D <folder>
# Writes docs/isolation-report-linux.md and docs/isolation-report-kubernetes.md;
# fails if a required check is missing or failing.
set -eu
[ $# -eq 1 ] && [ -d "$1" ] || { echo "usage: $0 <folder with the weekly run's artifacts>" >&2; exit 2; }
ARTIFACTS=$1
cd "$(dirname "$0")/.."
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# render <artifact name pattern> <findings folder in the artifact> <report> <how it was made>
render() {
	mkdir -p "$work/$2"
	found=0
	for d in "$ARTIFACTS"/$1/bin/"$2"/*/; do
		[ -d "$d" ] || continue
		cp -R "${d%/}" "$work/$2/"
		found=1
	done
	[ "$found" -eq 1 ] || { echo "no findings for $3 in $ARTIFACTS/$1" >&2; exit 1; }
	go run ./cmd/nbsandbox-report -in "$work/$2" -out "$3" -require -target "$4"
}

render 'isolation-report-docker-*' findings docs/isolation-report-linux.md \
	"make report in the weekly CI run (GitHub's Linux runner, Docker Engine, 2 GiB per run)"
render 'isolation-report-kubernetes-1.37-*' findings-k8s docs/isolation-report-kubernetes.md \
	"make report-k8s in the weekly CI run (kind with Kubernetes 1.37 and Cilium, on GitHub's Linux runner)"
echo "review the two reports (git diff docs/), then commit them in a pull request"
