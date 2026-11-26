#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# The Helm upgrade test (make test-helm-upgrade), on the local kind cluster:
# the chart of an earlier release (FROM, a tag), with its published
# nbsandboxd image and its own kind values, fills a fresh database and
# object store; this checkout's chart then upgrades the release in place, as
# an operator's `helm upgrade` would, with connections on. TestUpgrade checks
# that everything made before is still there and still works.
#
# Needs the kind cluster (make k8s-cluster) and these from the Makefile:
# KIND_NODE, KIND_CONFIG, KIND_DIR, HELM, GO. It resets the
# cluster's PostgreSQL and RustFS (their data is in emptyDir volumes) and
# leaves the release at this checkout's chart, as make k8s-up does. Runs get
# RUN_MEMORY (512 MiB) and RUN_CPUS (0.5) in both phases: with kind's
# defaults, two long runs and a session fill its node's 4 CPUs, and the
# upgrade's new worker Pods can't be scheduled until the runs end.
set -eu

FROM=${FROM:?set FROM to an earlier release tag, e.g. v0.3.0}; export FROM
FROM_IMAGE=ghcr.io/azbyluthfan/nbsandboxd:${FROM#v}
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/nbsandbox-helm-upgrade.XXXXXX")
old="$work/$FROM"

k() { docker exec -i "$KIND_NODE" kubectl --kubeconfig=/etc/kubernetes/admin.conf "$@"; }
helm_() { $HELM "$@" --kubeconfig "$KIND_CONFIG"; }

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		echo "helm upgrade test failed; the release's pods and logs:" >&2
		k -n nbsandbox get pods >&2 || true
		k -n nbsandbox logs -l app.kubernetes.io/instance=nbsandbox --prefix --tail=40 >&2 2>/dev/null || true
		if [ -s "$work/old-workers.log" ]; then
			echo "the $FROM workers' logs, as they stopped:" >&2
			grep -v '"http request"' "$work/old-workers.log" | tail -60 >&2
		fi
	fi
	git -C "$root" worktree remove --force "$old" >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT INT TERM

echo "== a fresh release from $FROM's chart"
helm_ uninstall nbsandbox -n nbsandbox --wait >/dev/null 2>&1 || true
k -n nbsandbox-runs delete pods,secrets,networkpolicies -l app.kubernetes.io/managed-by=nbsandbox --ignore-not-found >/dev/null 2>&1 || true
k -n nbsandbox rollout restart deploy/postgres deploy/rustfs >/dev/null # emptyDir: a fresh database and object store
k -n nbsandbox rollout status deploy/postgres deploy/rustfs --timeout=300s >/dev/null
# CI checks out one commit without tags: fetch the one this needs.
git -C "$root" rev-parse -q --verify "refs/tags/${FROM}" >/dev/null ||
	git -C "$root" fetch -q --depth=1 origin "refs/tags/${FROM}:refs/tags/${FROM}"
git -C "$root" worktree add --detach "$old" "$FROM" >/dev/null
# The node pulls the published image itself (it's public): kind load can't
# import a multi-platform image Docker pulled for one platform.
docker exec "$KIND_NODE" crictl pull "$FROM_IMAGE" >/dev/null
helm_ install nbsandbox "$old/deploy/helm/nbsandbox" -n nbsandbox -f "$old/deploy/kubernetes/kind/values.yaml" \
	--set image.repository="${FROM_IMAGE%:*}" --set image.tag="${FROM#v}" --set image.pullPolicy=IfNotPresent \
	--set runs.memory="${RUN_MEMORY:-512MiB}" --set runs.cpus="${RUN_CPUS:-0.5}" \
	--wait --timeout 5m >/dev/null
k -n nbsandbox rollout status deploy/nbsandbox-api deploy/nbsandbox-worker --timeout=180s >/dev/null

echo "== $FROM: make the data"
NBSANDBOX_UPGRADE=before NBSANDBOX_UPGRADE_STATE="$work/state.json" NBSANDBOX_CLUSTER=1 NBSANDBOX_K8S=1 NBSANDBOX_KIND_NODE="$KIND_NODE" \
	"$GO" test -C "$root" -tags integration -count=1 -timeout 10m -run '^TestUpgrade$' ${TESTFLAGS:-} ./tests/integration/

# kind's one node has 4 CPUs: no room for a new worker (and its network
# probe) beside the old ones' runs. Replace workers one at a time, as a
# full cluster would (the chart sets no strategy, so helm upgrade keeps it).
k -n nbsandbox patch deployment nbsandbox-worker -p '{"spec":{"strategy":{"rollingUpdate":{"maxSurge":0,"maxUnavailable":1}}}}' >/dev/null

# The old workers' logs, kept: they decide what becomes of their runs and
# sessions as they stop, and their Pods go with the upgrade.
k -n nbsandbox logs -f -l app.kubernetes.io/component=worker --prefix --max-log-requests 10 --tail=-1 > "$work/old-workers.log" 2>&1 &
follow=$!

echo "== this checkout's chart: helm upgrade in place, connections on"
helm_ upgrade nbsandbox "$root/deploy/helm/nbsandbox" -n nbsandbox -f "$KIND_DIR/values.yaml" ${HELM_SET:-} --wait --timeout 5m >/dev/null
k -n nbsandbox rollout status deploy/nbsandbox-api deploy/nbsandbox-worker --timeout=180s >/dev/null
kill "$follow" 2>/dev/null || true
NBSANDBOX_UPGRADE=after NBSANDBOX_UPGRADE_STATE="$work/state.json" NBSANDBOX_CLUSTER=1 NBSANDBOX_K8S=1 NBSANDBOX_KIND_NODE="$KIND_NODE" \
	NBSANDBOX_TEST_CONNECTIONS=1 "$GO" test -C "$root" -tags integration -count=1 -timeout 15m -run '^TestUpgrade$' ${TESTFLAGS:-} ./tests/integration/
