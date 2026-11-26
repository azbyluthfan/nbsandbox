// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

func fetcherSpec() isolation.RunSpec {
	s := testSpec()
	s.Fetcher = &isolation.Fetcher{Image: "nbsandbox-fetcher:dev", Config: []byte(`{"sources":[{"secret":"pa55"}]}`),
		Token: "fetcher-token", Gateway: "nbsandbox-gateway"}
	return s
}

// The runner with a fetcher: its run's network instead of none, told where
// the fetcher is; never the credentials.
func TestRunnerSpecWithFetcher(t *testing.T) {
	spec := fetcherSpec()
	cfg, host := buildContainerConfig(spec)
	if string(host.NetworkMode) != NetworkName(spec.RunID) {
		t.Fatalf("network mode %q", host.NetworkMode)
	}
	if !slices.Contains(cfg.Env, "NBSANDBOX_FETCHER=grpc://"+FetcherName(spec.RunID)+":8815") ||
		!slices.Contains(cfg.Env, "NBSANDBOX_FETCHER_TOKEN=fetcher-token") {
		t.Fatalf("env %v", cfg.Env)
	}
	for _, e := range cfg.Env {
		if strings.Contains(e, "pa55") {
			t.Fatal("the runner got the credentials")
		}
	}
}

// The fetcher: the run's hardening, the run's network only, no files.
func TestFetcherSpec(t *testing.T) {
	spec := fetcherSpec()
	spec.Runtime = "runsc"
	cfg, host := buildFetcherConfig(spec, runFetcher)
	rows := map[string]bool{
		"image":               cfg.Image == "nbsandbox-fetcher:dev",
		"user 1000":           cfg.User == "1000:1000",
		"the run's network":   string(host.NetworkMode) == NetworkName(spec.RunID),
		"no files":            len(host.Binds) == 0 && len(host.Mounts) == 0 && len(host.VolumesFrom) == 0,
		"capabilities":        slices.Equal(host.CapDrop, []string{"ALL"}) && len(host.CapAdd) == 0 && !host.Privileged,
		"no new privileges":   slices.Contains(host.SecurityOpt, "no-new-privileges"),
		"read-only root":      host.ReadonlyRootfs && host.Tmpfs["/tmp"] != "",
		"limits":              host.Memory == spec.Limits.MemoryBytes && host.MemorySwap == host.Memory && host.NanoCPUs == spec.Limits.NanoCPUs,
		"gVisor, its pids":    host.Runtime == "runsc" && len(host.Ulimits) == 1,
		"configuration":       slices.Contains(cfg.Env, "NBSANDBOX_FETCHER_CONFIG_JSON="+string(spec.Fetcher.Config)),
		"not a run container": cfg.Labels[LabelRun] == "" && cfg.Labels[LabelFetcher] == spec.RunID,
	}
	for name, ok := range rows {
		if !ok {
			t.Errorf("%s: %+v %+v", name, cfg, host)
		}
	}
}

func TestRunWithFetcher(t *testing.T) {
	f := newFakeDocker()
	f.fetcherLogs = multiplex("", "fetcher: serving 2 source(s) on grpc://0.0.0.0:8815\n")
	f.exit <- 0
	spec := fetcherSpec()
	var out lines
	res, err := newTestBackend(f).Run(context.Background(), spec, &out)
	if err != nil || !res.Removed {
		t.Fatalf("run: %+v %v", res, err)
	}
	net := NetworkName(spec.RunID)
	calls := f.callList()
	order := []string{"network-create " + net, "network-connect " + net + " nbsandbox-gateway", "fetcher-create", "fetcher-start",
		"fetcher-logs", "create", "start", "remove", "fetcher-remove", "network-disconnect " + net + " nbsandbox-gateway", "network-remove " + net}
	at := -1
	for _, c := range order {
		i := slices.Index(calls, c)
		if i <= at {
			t.Fatalf("%q out of order in %v", c, calls)
		}
		at = i
	}
	if got := out.all(); len(got) == 0 || !strings.HasPrefix(got[0], "[fetcher] fetcher: serving") {
		t.Fatalf("lines %q", got)
	}
	// Names in /etc/hosts, not Docker's DNS (gVisor can't reach it): the
	// gateway's address for the fetcher, the fetcher's for the runner.
	if h := f.fetcherCreated.host.ExtraHosts; !slices.Contains(h, "nbsandbox-gateway:10.5.0.2") {
		t.Fatalf("fetcher's hosts %v", h)
	}
	if h := f.created.host.ExtraHosts; !slices.Contains(h, FetcherName(spec.RunID)+":10.5.0.3") {
		t.Fatalf("runner's hosts %v", h)
	}
}

func TestFetcherFailsToStart(t *testing.T) {
	f := newFakeDocker()
	f.fetcherLogs = multiplex("", "Traceback (most recent call last):\nduckdb.IOException: IO Error: Unable to connect to Postgres\n")
	spec := fetcherSpec()
	var out lines
	_, err := newTestBackend(f).Run(context.Background(), spec, &out)
	if !errors.Is(err, isolation.ErrStart) || !strings.Contains(err.Error(), "Unable to connect to Postgres") {
		t.Fatalf("err %v", err)
	}
	calls := f.callList()
	if slices.Contains(calls, "create") {
		t.Fatal("the runner started without its fetcher")
	}
	if !slices.Contains(calls, "fetcher-remove") || !slices.Contains(calls, "network-remove "+NetworkName(spec.RunID)) {
		t.Fatalf("not cleaned up: %v", calls)
	}
}

func TestOrphanFetchersAndNetworks(t *testing.T) {
	f := newFakeDocker()
	f.containers = []ContainerSummary{{ID: "c1", Labels: map[string]string{LabelRun: "r1", LabelSpace: "s1"}, Running: true}}
	f.fetchers = []ContainerSummary{
		{ID: "f1", Labels: map[string]string{LabelFetcher: "r1"}},
		{ID: "f2", Labels: map[string]string{LabelFetcher: "r2", LabelSpace: "s2"}},
	}
	f.networks = []NetworkSummary{
		{ID: "n2", Labels: map[string]string{LabelNetwork: "r2"}},
		{ID: "n3", Labels: map[string]string{LabelNetwork: "r3", LabelGateway: "gw"}},
	}
	b := newTestBackend(f)
	orphans, err := b.Orphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, o := range orphans {
		ids = append(ids, o.RunID)
	}
	if !slices.Equal(ids, []string{"r1", "r2", "r3"}) || !orphans[0].Running || orphans[1].Running || orphans[1].SpaceID != "s2" {
		t.Fatalf("orphans %+v", orphans)
	}
	if err := b.Kill(context.Background(), "r3"); err != nil {
		t.Fatal(err)
	}
	calls := f.callList()
	if !slices.Contains(calls, "network-disconnect n3 gw") || !slices.Contains(calls, "network-remove n3") {
		t.Fatalf("calls %v", calls)
	}
}

func TestFetcherOwnLimits(t *testing.T) {
	spec := fetcherSpec()
	spec.Fetcher.MemoryBytes, spec.Fetcher.NanoCPUs = 512<<20, 5e8
	cfg, host := buildFetcherConfig(spec, runFetcher)
	if host.Memory != 512<<20 || host.MemorySwap != host.Memory || host.NanoCPUs != 5e8 ||
		!slices.Contains(cfg.Env, "NBSANDBOX_FETCHER_MEMORY=536870912") {
		t.Fatalf("limits: %d %d %v", host.Memory, host.NanoCPUs, cfg.Env)
	}
}

func TestFetcherHostAliases(t *testing.T) {
	spec := fetcherSpec()
	spec.Fetcher.Hosts = map[string]string{"my.internal": "127.0.0.2", "a.internal": "127.0.0.3"}
	_, host := buildFetcherConfig(spec, runFetcher)
	if !slices.Equal(host.ExtraHosts, []string{"a.internal:127.0.0.3", "my.internal:127.0.0.2"}) {
		t.Fatalf("extra hosts: %v", host.ExtraHosts)
	}
	if _, plain := buildFetcherConfig(fetcherSpec(), runFetcher); len(plain.ExtraHosts) != 0 {
		t.Fatalf("extra hosts without aliases: %v", plain.ExtraHosts)
	}
}
