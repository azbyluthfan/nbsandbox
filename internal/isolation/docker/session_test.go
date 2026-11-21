// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

func sessionSpec() isolation.SessionSpec {
	return isolation.SessionSpec{
		SessionID: "33333333-3333-4333-8333-333333333333", SpaceID: "22222222-2222-4222-8222-222222222222",
		SpaceDir: "/data/spaces/s/work/x", Image: "nbsandbox-runner:test", MaxMessage: 2 << 20,
		Limits: isolation.Limits{MemoryBytes: 1 << 30, NanoCPUs: 1e9, PIDs: 256, TmpfsBytes: 1 << 28, ShmBytes: 1 << 26, LogBytes: 1 << 20},
	}
}

// A session gets a run's isolation: these are the run's controls, checked
// again on the session's config.
func TestSessionConfigKeepsTheRunsIsolation(t *testing.T) {
	cfg, host := buildSessionConfig(sessionSpec())
	if host.NetworkMode != "none" || !host.ReadonlyRootfs || host.Privileged || !slices.Equal(host.CapDrop, []string{"ALL"}) ||
		!slices.Contains(host.SecurityOpt, "no-new-privileges") || cfg.User != "1000:1000" || host.IpcMode != "private" {
		t.Fatalf("isolation controls: %+v %+v", cfg, host)
	}
	if host.Memory != 1<<30 || host.NanoCPUs != 1e9 || *host.PidsLimit != 256 || host.MemorySwap != 1<<30 {
		t.Fatalf("limits: %+v", host.Resources)
	}
	if !strings.Contains(host.Tmpfs["/tmp"], "size=268435456") {
		t.Fatalf("tmpfs %v", host.Tmpfs)
	}
}

func TestSessionConfigRunsTheBridgeAttached(t *testing.T) {
	cfg, host := buildSessionConfig(sessionSpec())
	if !slices.Equal(cfg.Entrypoint, []string{"python", isolation.SessionBridge}) || len(cfg.Cmd) != 0 {
		t.Fatalf("entrypoint %v cmd %v", cfg.Entrypoint, cfg.Cmd)
	}
	if !cfg.OpenStdin || cfg.StdinOnce || !cfg.AttachStdin || !cfg.AttachStdout {
		t.Fatalf("stdin: %+v", cfg)
	}
	if host.LogConfig.Type != "none" {
		t.Fatalf("the protocol must not be stored as a log: %v", host.LogConfig)
	}
	if cfg.Labels[LabelSession] != sessionSpec().SessionID || cfg.Labels[LabelRun] != "" {
		t.Fatalf("labels %v: a session must not look like a run to the run reconciler", cfg.Labels)
	}
	for _, e := range cfg.Env {
		if strings.HasPrefix(e, "NBSANDBOX_RUN") || strings.HasPrefix(e, "NBSANDBOX_OUTPUT") {
			t.Fatalf("run environment in a session: %v", cfg.Env)
		}
	}
	if !slices.Contains(cfg.Env, "NBSANDBOX_SESSION_MAX_MESSAGE=2097152") {
		t.Fatalf("env %v", cfg.Env)
	}
}

func TestSessionConfigMounts(t *testing.T) {
	spec := sessionSpec()
	_, host := buildSessionConfig(spec)
	if !slices.Equal(host.Binds, []string{spec.SpaceDir + ":/space:rw"}) {
		t.Fatalf("read-write session binds %v (no /run)", host.Binds)
	}
	spec.SpaceReadOnly, spec.PackagesDir = true, "/data/packages/k"
	_, host = buildSessionConfig(spec)
	if !slices.Equal(host.Binds, []string{spec.SpaceDir + ":/space:ro", "/data/packages/k:" + isolation.PackagesPath + ":ro"}) {
		t.Fatalf("read-only session binds %v", host.Binds)
	}
	spec.Runtime = "runsc"
	_, host = buildSessionConfig(spec)
	if host.Runtime != "runsc" || len(host.Ulimits) != 1 {
		t.Fatalf("gVisor: %v %v", host.Runtime, host.Ulimits)
	}
}

// pipeConn is a fake attach: the test writes the container's multiplexed
// output and reads what was written to its stdin.
type pipeConn struct {
	stdin  *io.PipeWriter
	stdinR *io.PipeReader
	out    *io.PipeWriter
	outR   *io.PipeReader
}

func newPipeConn() *pipeConn {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &pipeConn{stdin: inW, stdinR: inR, out: outW, outR: outR}
}

func (p *pipeConn) attached() Attached { return Attached{Conn: p.stdin, Reader: p.outR} }

func TestStartSessionAttachesBeforeStarting(t *testing.T) {
	f := newFakeDocker()
	pc := newPipeConn()
	f.attach = func(string) (Attached, error) { return pc.attached(), nil }
	b := newTestBackend(f)
	conn, err := b.StartSession(context.Background(), sessionSpec())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.callList(); !slices.Equal(got, []string{"create", "attach", "start"}) {
		t.Fatalf("calls %v", got)
	}
	if f.created.name != SessionContainerName(sessionSpec().SessionID) {
		t.Fatalf("name %q", f.created.name)
	}
	// stdout is demultiplexed; stderr isn't mixed in.
	go func() {
		_, _ = pc.out.Write(multiplex("", "Traceback\n"))
		_, _ = pc.out.Write(multiplex(`{"channel":"bridge","content":{"status":"ready"}}`+"\n", ""))
	}()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, `"ready"`) {
		t.Fatalf("read %q %v", line, err)
	}
	go func() { _, _ = conn.Write([]byte("{}\n")) }()
	got, _ := bufio.NewReader(pc.stdinR).ReadString('\n')
	if got != "{}\n" {
		t.Fatalf("stdin got %q", got)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after close")
	}
}

func TestStartSessionRemovesTheContainerWhenStartFails(t *testing.T) {
	f := newFakeDocker()
	pc := newPipeConn()
	f.attach = func(string) (Attached, error) { return pc.attached(), nil }
	f.startErr = errors.New("no such image")
	_, err := newTestBackend(f).StartSession(context.Background(), sessionSpec())
	if !errors.Is(err, isolation.ErrStart) {
		t.Fatalf("err %v", err)
	}
	if got := f.callList(); !slices.Equal(got, []string{"create", "attach", "start", "remove"}) {
		t.Fatalf("calls %v", got)
	}
}

func TestSessionsListsByLabel(t *testing.T) {
	f := newFakeDocker()
	f.containers = []ContainerSummary{{ID: "c1", Running: true, Labels: map[string]string{LabelSession: "s1", LabelSpace: "sp"}}}
	got, err := newTestBackend(f).Sessions(context.Background())
	if err != nil || len(got) != 1 || got[0] != (isolation.SessionInfo{SessionID: "s1", SpaceID: "sp", Running: true}) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestLineLoggerCapsStderr(t *testing.T) {
	var lines []string
	l := &lineLogger{log: func(s string) { lines = append(lines, s) }}
	_, _ = l.Write([]byte("a\nb"))
	_, _ = l.Write([]byte("c\n"))
	if !slices.Equal(lines, []string{"a", "bc"}) {
		t.Fatalf("%q", lines)
	}
	for range 100 {
		_, _ = l.Write([]byte("x\n"))
	}
	if len(lines) != maxStderrLines+1 || lines[len(lines)-1] != "(more stderr not logged)" {
		t.Fatalf("%d lines, last %q", len(lines), lines[len(lines)-1])
	}
}
