#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Installs gVisor in a local kind cluster's node and adds a RuntimeClass
# "gvisor", so run Pods can use it (runs.runtimeClass: gvisor). Idempotent.
#
#   kind-gvisor.sh <node container> [version, default deploy/gvisor.version]
#
# The binaries come from gVisor's release bucket for exactly this version
# (the apt repository keeps only the newest release, so a pinned one drops
# out of its index), checked against the release's SHA-512. They are
# unpacked on this machine, which has bzip2 (the kind node doesn't), and
# copied into the node. A kind node is a container: what's installed stays
# while the node is kept (stop/start), and goes with `make k8s-down`.
set -eu
cd "$(dirname "$0")/.."
NODE=$1
VERSION=${2:-$(cat deploy/gvisor.version)}
node() { docker exec -i "$NODE" "$@"; }

if node sh -c "command -v runsc >/dev/null && runsc --version | grep -q 'release-$VERSION' && [ -d \$(dirname \$(command -v runsc))/gvisor-bin ]"; then
	echo "gVisor $VERSION already in $NODE"
else
	echo "installing gVisor $VERSION in $NODE"
	case $(node uname -m) in aarch64 | arm64) arch=aarch64 ;; x86_64) arch=x86_64 ;; *) echo "unknown arch" >&2; exit 1 ;; esac
	base="https://storage.googleapis.com/gvisor/releases/release/$VERSION/$arch"
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' EXIT
	curl -fsSL -o "$dir/gvisor.tar.bz2" "$base/gvisor.tar.bz2" || { echo "gVisor $VERSION not found for $arch" >&2; exit 1; }
	curl -fsSL -o "$dir/gvisor.tar.bz2.sha512" "$base/gvisor.tar.bz2.sha512"
	if command -v sha512sum >/dev/null; then
		(cd "$dir" && sha512sum -c gvisor.tar.bz2.sha512)
	else
		(cd "$dir" && shasum -a 512 -c gvisor.tar.bz2.sha512)
	fi
	mkdir "$dir/x"
	tar -xjf "$dir/gvisor.tar.bz2" -C "$dir/x"
	# runsc, the containerd shim, and the helpers runsc expects beside it (gvisor-bin/).
	docker cp "$dir/x/." "$NODE:/usr/local/bin/"
	node chmod -R a+rX /usr/local/bin/gvisor-bin
	node sh -c 'chmod 0755 /usr/local/bin/runsc /usr/local/bin/containerd-shim-runsc-v1'
fi

# containerd: the runsc handler, with systemd cgroups like runc here.
if ! node grep -q 'runtimes.runsc]' /etc/containerd/config.toml; then
	node sh -c 'cat >> /etc/containerd/config.toml' <<'EOF'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "/etc/containerd/runsc.toml"
EOF
	node sh -c 'cat > /etc/containerd/runsc.toml' <<'EOF'
[runsc_config]
  systemd-cgroup = "true"
EOF
	node systemctl restart containerd
	echo "registered runsc with containerd"
fi

# The RuntimeClass (cluster-wide).
node kubectl --kubeconfig=/etc/kubernetes/admin.conf apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF

# Check: a Pod under gVisor reports gVisor's kernel.
for _ in $(seq 30); do node kubectl --kubeconfig=/etc/kubernetes/admin.conf get --raw /readyz >/dev/null 2>&1 && break; sleep 2; done
node kubectl --kubeconfig=/etc/kubernetes/admin.conf delete pod gvisor-check --ignore-not-found >/dev/null
node kubectl --kubeconfig=/etc/kubernetes/admin.conf run gvisor-check --image=nbsandbox-runner:dev --image-pull-policy=Never \
	--restart=Never --overrides='{"spec":{"runtimeClassName":"gvisor"}}' --command -- sh -c 'dmesg | head -1' >/dev/null
node kubectl --kubeconfig=/etc/kubernetes/admin.conf wait --for=jsonpath='{.status.phase}'=Succeeded pod/gvisor-check --timeout=120s >/dev/null
echo "gvisor-check: $(node kubectl --kubeconfig=/etc/kubernetes/admin.conf logs gvisor-check)"
node kubectl --kubeconfig=/etc/kubernetes/admin.conf delete pod gvisor-check >/dev/null
