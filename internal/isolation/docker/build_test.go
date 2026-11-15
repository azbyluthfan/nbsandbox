// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

func buildSpec() isolation.BuildSpec {
	return isolation.BuildSpec{ID: "k1-abc", Worker: "w1", InDir: "/data/tmp/b/in", OutDir: "/data/tmp/b/out",
		Image: "nbsandbox-runner-builder:test", Network: "bridge",
		Limits: isolation.Limits{MemoryBytes: 2 << 30, NanoCPUs: 1e9, PIDs: 256, TmpfsBytes: 1 << 30, LogBytes: 1 << 20}}
}

// The build container has a run's hardening, network, and no space files.
func TestBuilderContainerSpec(t *testing.T) {
	cfg, host := buildBuilderConfig(buildSpec())
	if cfg.User != "1000:1000" || cfg.Image != "nbsandbox-runner-builder:test" || cfg.Labels[LabelBuild] != "k1-abc" || cfg.Labels[LabelBuildWorker] != "w1" {
		t.Errorf("config: %+v", cfg)
	}
	if _, isRun := cfg.Labels[LabelRun]; isRun {
		t.Error("a build must not look like a run to the run reconciler")
	}
	for _, e := range cfg.Env {
		if !strings.HasPrefix(e, "HOME=") && !strings.HasPrefix(e, "PYTHONUNBUFFERED=") && !strings.HasPrefix(e, "PIP_NO_CACHE_DIR=") {
			t.Errorf("unexpected environment %q", e)
		}
	}
	if !slices.Equal(host.Binds, []string{"/data/tmp/b/in:/in:ro", "/data/tmp/b/out:/out:rw"}) {
		t.Errorf("mounts: %v (only the request and the output)", host.Binds)
	}
	if host.NetworkMode != "bridge" || host.Privileged || !slices.Equal(host.CapDrop, []string{"ALL"}) ||
		!slices.Contains(host.SecurityOpt, "no-new-privileges") || !host.ReadonlyRootfs || host.IpcMode != "private" {
		t.Errorf("host: %+v", host)
	}
	if host.Memory != 2<<30 || host.MemorySwap != 2<<30 || host.NanoCPUs != 1e9 || *host.PidsLimit != 256 {
		t.Errorf("limits: %+v", host.Resources)
	}
	spec := buildSpec()
	spec.Runtime = "runsc"
	if _, h := buildBuilderConfig(spec); h.Runtime != "runsc" || len(h.Ulimits) != 1 {
		t.Errorf("gVisor: %+v", h)
	}
}

func TestRunMountsPackages(t *testing.T) {
	spec := testSpec()
	_, host := buildContainerConfig(spec)
	if len(host.Binds) != 2 {
		t.Fatalf("no set: %v", host.Binds)
	}
	spec.PackagesDir = "/data/packages/k1"
	_, host = buildContainerConfig(spec)
	if !slices.Contains(host.Binds, "/data/packages/k1:"+isolation.PackagesPath+":ro") {
		t.Errorf("package set mount: %v", host.Binds)
	}
}

func TestBuildPackagesAndOrphans(t *testing.T) {
	f := newFakeDocker()
	f.exit <- 0
	var out lines
	f.logs = multiplex("== installing from requirements.txt\n", "")
	res, err := newTestBackend(f).BuildPackages(context.Background(), buildSpec(), &out)
	if err != nil || res.ExitCode != 0 || !res.Removed || f.created.name != BuildContainerName("k1-abc") {
		t.Fatalf("build: %+v %v (%s)", res, err, f.created.name)
	}
	if got := strings.Join(out.all(), "|"); got != "== installing from requirements.txt" {
		t.Errorf("log %q", got)
	}
	// Only this worker's leftovers are removed.
	f = newFakeDocker()
	f.containers = []ContainerSummary{{ID: "mine", Labels: map[string]string{LabelBuild: "a", LabelBuildWorker: "w1"}},
		{ID: "theirs", Labels: map[string]string{LabelBuild: "b", LabelBuildWorker: "w2"}}}
	if err := newTestBackend(f).RemoveBuildOrphans(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); strings.Count(strings.Join(calls, ","), "remove") != 1 {
		t.Errorf("calls %v: only w1's container", calls)
	}
}
