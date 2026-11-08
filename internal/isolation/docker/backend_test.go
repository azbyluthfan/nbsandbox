// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

func testSpec() isolation.RunSpec {
	return isolation.RunSpec{
		RunID: "11111111-1111-4111-8111-111111111111", SpaceID: "22222222-2222-4222-8222-222222222222",
		SpaceDir: "/srv/nbsandbox/spaces/22222222-2222-4222-8222-222222222222/files",
		RunDir:   "/srv/nbsandbox/spaces/22222222-2222-4222-8222-222222222222/runs/11111111-1111-4111-8111-111111111111",
		Notebook: "forecast.ipynb",
		Image:    "nbsandbox-runner:dev", Runtime: "", MaxOutputBytes: 1 << 20,
		Limits: isolation.Limits{
			MemoryBytes: 2 << 30, NanoCPUs: 1e9, PIDs: 256, TmpfsBytes: 256 << 20, ShmBytes: 64 << 20,
			LogBytes: 10 << 20, StopGrace: 50 * time.Millisecond,
		},
	}
}

// One test per isolation control: removing any control fails
// the test named after it.
func TestContainerSpec(t *testing.T) {
	spec := testSpec()
	cfg, host := buildContainerConfig(spec)
	rows := map[string]func() bool{
		"mounts: only the space's files at /space and this run's folder at /run": func() bool {
			return slices.Equal(host.Binds, []string{spec.SpaceDir + ":/space:rw", spec.RunDir + ":/run:rw"}) &&
				len(host.Mounts) == 0 && len(host.VolumesFrom) == 0
		},
		"process namespace: private": func() bool { return host.PidMode == "" },
		"init process: enabled":      func() bool { return host.Init != nil && *host.Init },
		"memory and swap: 2 GB, no swap": func() bool {
			return host.Memory == 2<<30 && host.MemorySwap == host.Memory
		},
		"cpu: 1 CPU":      func() bool { return host.NanoCPUs == 1e9 },
		"pids limit: 256": func() bool { return host.PidsLimit != nil && *host.PidsLimit == 256 },
		"shared memory: 64 MB, private": func() bool {
			return host.ShmSize == 64<<20 && string(host.IpcMode) == "private"
		},
		"network mode: none": func() bool { return string(host.NetworkMode) == "none" },
		"user: 1000:1000":    func() bool { return cfg.User == "1000:1000" },
		"capabilities: all dropped": func() bool {
			return slices.Equal(host.CapDrop, []string{"ALL"}) && len(host.CapAdd) == 0
		},
		"security options: no-new-privileges, default seccomp and AppArmor": func() bool {
			for _, o := range host.SecurityOpt {
				if strings.Contains(o, "unconfined") {
					return false
				}
			}
			return slices.Contains(host.SecurityOpt, "no-new-privileges")
		},
		"not privileged": func() bool { return !host.Privileged && len(host.Devices) == 0 },
		"root filesystem: read-only, tmpfs /tmp 256 MB, HOME=/tmp": func() bool {
			return host.ReadonlyRootfs && host.Tmpfs["/tmp"] == "rw,nosuid,nodev,size=268435456" &&
				slices.Contains(cfg.Env, "HOME=/tmp")
		},
		"docker log driver: size-capped": func() bool {
			c := host.LogConfig.Config
			return host.LogConfig.Type == "local" && c["max-size"] == "10485760" && c["max-file"] == "1" && c["compress"] == "false"
		},
		"environment: allowlist only": func() bool {
			want := []string{"HOME=/tmp", "PYTHONUNBUFFERED=1", "NBSANDBOX_RUN_ID=" + spec.RunID,
				"NBSANDBOX_RUN_DIR=/run", "NBSANDBOX_OUTPUT_DIR=/run/artifacts", "NBSANDBOX_MAX_OUTPUT=1048576", "NBSANDBOX_OUTPUT_MODE=single",
				"NBSANDBOX_LOG_CAP=10485760"}
			return slices.Equal(cfg.Env, want)
		},
		"docker socket: not mounted": func() bool {
			for _, b := range host.Binds {
				if strings.Contains(b, "docker.sock") {
					return false
				}
			}
			return true
		},
		"runtime: as configured": func() bool { return host.Runtime == "" },
		"labels: run and space":  func() bool { return cfg.Labels[LabelRun] == spec.RunID && cfg.Labels[LabelSpace] == spec.SpaceID },
		"working directory: /space, notebook argument": func() bool {
			return cfg.WorkingDir == "/space" && slices.Equal(cfg.Cmd, []string{"forecast.ipynb"})
		},
		"not auto-removed (inspected first)": func() bool { return !host.AutoRemove },
	}
	for name, ok := range rows {
		t.Run(name, func(t *testing.T) {
			if !ok() {
				t.Fatalf("isolation control missing or wrong: %s", name)
			}
		})
	}
	spec.SpaceReadOnly = true
	if _, host := buildContainerConfig(spec); !slices.Equal(host.Binds, []string{spec.SpaceDir + ":/space:ro", spec.RunDir + ":/run:rw"}) {
		t.Fatalf("read-only space not mounted read-only: %v", host.Binds)
	}
	spec.Runtime = "runsc"
	_, host = buildContainerConfig(spec)
	if host.Runtime != "runsc" {
		t.Fatal("runsc not passed through")
	}
	// gVisor: the notebook's process limit is RLIMIT_NPROC, the host's pids
	// limit a backstop with room for gVisor's own threads.
	if len(host.Ulimits) != 1 || host.Ulimits[0].Name != "nproc" || host.Ulimits[0].Soft != 256 || host.Ulimits[0].Hard != 256 ||
		host.PidsLimit == nil || *host.PidsLimit != 4*256+256 {
		t.Fatalf("gVisor process limits: %+v pids %v", host.Ulimits, host.PidsLimit)
	}
	spec.Runtime = ""
	if _, host := buildContainerConfig(spec); len(host.Ulimits) != 0 {
		t.Fatal("RLIMIT_NPROC set under runc")
	}
}

type lines struct {
	mu sync.Mutex
	l  []string
}

func (l *lines) Line(s string) { l.mu.Lock(); l.l = append(l.l, s); l.mu.Unlock() }
func (l *lines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.l...)
}

func newTestBackend(f *fakeDocker) *Backend { return NewBackend(f, quietLogger()) }

func TestRunNormalExit(t *testing.T) {
	f := newFakeDocker()
	f.logs = multiplex("", "line one\nline two\r\npartial")
	f.inspect = ContainerState{ExitCode: 10, OOMKilled: true}
	f.exit <- 10
	var out lines
	res, err := newTestBackend(f).Run(context.Background(), testSpec(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 10 || !res.OOMKilled || res.Stopped || !res.Removed {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(out.all(), "|"); got != "line one|line two|partial" {
		t.Fatalf("lines = %q", got)
	}
	if f.created.name != ContainerName(testSpec().RunID) {
		t.Fatalf("container name %q", f.created.name)
	}
	if calls := f.callList(); !slices.Contains(calls, "remove") || slices.Contains(calls, "stop") {
		t.Fatalf("calls %v", calls)
	}
}

// After cancellation, stop/inspect/remove use a fresh context.
func TestCancelStopsWithFreshContext(t *testing.T) {
	f := newFakeDocker()
	f.stopExits = true
	f.inspect = ContainerState{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	res, err := newTestBackend(f).Run(ctx, testSpec(), &lines{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped || res.ExitCode != 143 || !res.Removed {
		t.Fatalf("result %+v", res)
	}
	calls := f.callList()
	for _, c := range []string{"stop", "inspect", "remove"} {
		if !slices.Contains(calls, c) {
			t.Fatalf("missing %s in %v", c, calls)
		}
	}
	if len(f.cancelledCtxs) > 0 {
		t.Fatalf("Docker calls made with a cancelled context: %v", f.cancelledCtxs)
	}
}

func TestCancelledBeforeStart(t *testing.T) {
	f := newFakeDocker()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := newTestBackend(f).Run(ctx, testSpec(), &lines{})
	if err != nil || !res.Stopped {
		t.Fatalf("res %+v err %v", res, err)
	}
	if slices.Contains(f.callList(), "start") || len(f.cancelledCtxs) > 0 {
		t.Fatalf("calls %v cancelled %v", f.callList(), f.cancelledCtxs)
	}
}

func TestStartFailuresArePlatformErrors(t *testing.T) {
	for name, set := range map[string]func(*fakeDocker){
		"create": func(f *fakeDocker) { f.createErr = errors.New("No such image: nbsandbox-runner:dev") },
		"start":  func(f *fakeDocker) { f.startErr = errors.New("runtime runsc not found") },
	} {
		f := newFakeDocker()
		set(f)
		_, err := newTestBackend(f).Run(context.Background(), testSpec(), &lines{})
		if !errors.Is(err, isolation.ErrStart) {
			t.Errorf("%s failure: %v, want ErrStart", name, err)
		}
		if name == "start" && !slices.Contains(f.callList(), "remove") {
			t.Error("container left behind after a failed start")
		}
	}
}

// Docker going away mid-run.
func TestDockerLostMidRun(t *testing.T) {
	f := newFakeDocker()
	f.waitErr <- errors.New("unexpected EOF")
	f.inspectErr = errors.New("dial unix docker.sock: connect: no such file or directory")
	_, err := newTestBackend(f).Run(context.Background(), testSpec(), &lines{})
	if !errors.Is(err, isolation.ErrDockerLost) {
		t.Fatalf("got %v, want ErrDockerLost", err)
	}
	if slices.Contains(f.callList(), "remove") {
		t.Fatal("must not remove when Docker is gone; reconciliation does it")
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := newLineWriter(func(s string) { got = append(got, s) })
	_, _ = w.Write([]byte("a\nb"))
	_, _ = w.Write([]byte("c\r\n\n"))
	long := strings.Repeat("x", MaxLineBytes+10)
	_, _ = w.Write([]byte(long + "\nend"))
	w.Flush()
	want := []string{"a", "bc", "", strings.Repeat("x", MaxLineBytes), "xxxxxxxxxx", "end"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %d lines %q..., want %d", len(got), got[:min(3, len(got))], len(want))
	}
}

func TestOrphansAndKill(t *testing.T) {
	f := newFakeDocker()
	f.containers = []ContainerSummary{{ID: "c1", Labels: map[string]string{LabelRun: "r1", LabelSpace: "s1"}, Running: true}}
	b := newTestBackend(f)
	orphans, err := b.Orphans(context.Background())
	if err != nil || len(orphans) != 1 || orphans[0].RunID != "r1" || orphans[0].SpaceID != "s1" || !orphans[0].Running {
		t.Fatalf("orphans %+v %v", orphans, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Kill(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if len(f.cancelledCtxs) > 0 {
		t.Fatalf("Kill used a cancelled context: %v", f.cancelledCtxs)
	}
}
