#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Installs gVisor in a local kind cluster's node and adds a RuntimeClass
# "gvisor", so run Pods can use it (runs.runtimeClass: gvisor). Idempotent.
#
#   kind-gvisor.sh <node container> [version, default deploy/gvisor.version]
#
# The binaries come from gVisor's apt repository, the package checked against
# the SHA-256 in the repository's index. A kind node is a container: what's installed stays
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
	# From gVisor's apt repository: its index (over HTTPS) gives the
	# package's path and SHA-256 for this version and architecture.
	node sh -eu -c "
		case \$(uname -m) in aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) echo unknown arch; exit 1 ;; esac
		repo=https://storage.googleapis.com/gvisor/releases
		entry=\$(curl -fsSL \$repo/dists/release/main/binary-\$arch/Packages | awk -v v=$VERSION '
			/^Version:/ {ver=\$2} /^Filename:/ {f=\$2} /^SHA256:/ {if (ver == v) print f, \$2}' | head -1)
		[ -n \"\$entry\" ] || { echo \"gVisor $VERSION not found for \$arch\" >&2; exit 1; }
		set -- \$entry
		dir=\$(mktemp -d)
		curl -fsSL -o \$dir/runsc.deb \$repo/\$1
		echo \"\$2  \$dir/runsc.deb\" | sha256sum -c -
		dpkg-deb -x \$dir/runsc.deb \$dir/x
		# runsc, the containerd shim, and the helpers runsc expects beside it (gvisor-bin/).
		cp -a \$dir/x/usr/bin/. /usr/local/bin/
		rm -r \$dir
	"
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
for i in $(seq 30); do node kubectl --kubeconfig=/etc/kubernetes/admin.conf get --raw /readyz >/dev/null 2>&1 && break; sleep 2; done
node kubectl --kubeconfig=/etc/kubernetes/admin.conf delete pod gvisor-check --ignore-not-found >/dev/null
node kubectl --kubeconfig=/etc/kubernetes/admin.conf run gvisor-check --image=nbsandbox-runner:dev --image-pull-policy=Never \
	--restart=Never --overrides='{"spec":{"runtimeClassName":"gvisor"}}' --command -- sh -c 'dmesg | head -1' >/dev/null
node kubectl --kubeconfig=/etc/kubernetes/admin.conf wait --for=jsonpath='{.status.phase}'=Succeeded pod/gvisor-check --timeout=120s >/dev/null
echo "gvisor-check: $(node kubectl --kubeconfig=/etc/kubernetes/admin.conf logs gvisor-check)"
node kubectl --kubeconfig=/etc/kubernetes/admin.conf delete pod gvisor-check >/dev/null
