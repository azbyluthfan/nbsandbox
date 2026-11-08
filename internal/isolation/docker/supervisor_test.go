// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClient simulates Docker going away and coming back.
type fakeClient struct {
	noContainers
	mu       sync.Mutex
	down     bool
	runtimes []string
	noImage  bool
	pings    atomic.Int32
}

func (f *fakeClient) set(fn func(*fakeClient)) { f.mu.Lock(); fn(f); f.mu.Unlock() }

func (f *fakeClient) Ping(context.Context) error {
	f.pings.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errors.New("dial unix docker.sock: connect: no such file or directory")
	}
	return nil
}

func (f *fakeClient) Info(context.Context) (Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Info{ServerVersion: "29.9.2", Runtimes: f.runtimes}, nil
}

func (f *fakeClient) ImageInspect(_ context.Context, ref string) (Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noImage {
		return Image{}, errors.New("No such image: " + ref)
	}
	return Image{ID: "sha256:abc", Labels: map[string]string{"io.nbsandbox.manifest": `{"status_schema_version":2}`}}, nil
}

func (f *fakeClient) Close() error { return nil }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestSupervisor(c Client, runtime string, onChange func(bool)) *Supervisor {
	return NewSupervisor(c, SupervisorConfig{
		Image: "nbsandbox-runner:dev", Runtime: runtime,
		UpInterval: 5 * time.Millisecond, MinBackoff: 2 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
		CallTimeout: time.Second, OnChange: onChange, Logger: quietLogger(),
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Docker going down and coming back is followed without a restart.
func TestSupervisorFollowsDockerDownAndUp(t *testing.T) {
	fc := &fakeClient{runtimes: []string{"runc"}}
	var mu sync.Mutex
	var changes []bool
	s := newTestSupervisor(fc, "", func(up bool) { mu.Lock(); changes = append(changes, up); mu.Unlock() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, "up", func() bool { return s.Status().Up })
	if err := s.DockerCheck(ctx); err != nil {
		t.Fatalf("docker check while up: %v", err)
	}

	fc.set(func(f *fakeClient) { f.down = true })
	waitFor(t, "down", func() bool { return !s.Status().Up })
	if err := s.DockerCheck(ctx); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("docker check while down: %v", err)
	}
	if err := s.ImageCheck(ctx); err == nil {
		t.Fatal("image check must not pass while docker is down")
	}

	fc.set(func(f *fakeClient) { f.down = false })
	waitFor(t, "up again", func() bool { return s.Status().Up })
	// The status flips before OnChange runs; wait for the callback too.
	waitFor(t, "third OnChange", func() bool { mu.Lock(); defer mu.Unlock(); return len(changes) >= 3 })

	mu.Lock()
	defer mu.Unlock()
	if len(changes) != 3 || !changes[0] || changes[1] || !changes[2] {
		t.Fatalf("OnChange calls = %v, want [true false true]", changes)
	}
}

func TestSupervisorBacksOffWhileDown(t *testing.T) {
	fc := &fakeClient{down: true}
	s := NewSupervisor(fc, SupervisorConfig{
		Image: "img", MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond, Logger: quietLogger(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	// Without backoff a 300 ms window would allow ~30 pings at 10 ms; with
	// backoff to 40 ms it is 10, 20, 40, 40, ... → about 9.
	if n := fc.pings.Load(); n < 4 || n > 14 {
		t.Fatalf("pings while down = %d, want backoff (4..14)", n)
	}
}

func TestStartupFailsOnMissingRuntimeWhenDockerReachable(t *testing.T) {
	fc := &fakeClient{runtimes: []string{"io.containerd.runc.v2", "runc"}}
	s := newTestSupervisor(fc, "runsc", nil)
	err := s.CheckStartup(context.Background())
	if !errors.Is(err, ErrRuntimeMissing) || !strings.Contains(err.Error(), "runsc") {
		t.Fatalf("got %v, want ErrRuntimeMissing naming runsc", err)
	}
}

func TestStartupDefersWhenDockerUnreachable(t *testing.T) {
	s := newTestSupervisor(&fakeClient{down: true}, "runsc", nil)
	if err := s.CheckStartup(context.Background()); err != nil {
		t.Fatalf("unreachable docker must not fail startup: %v", err)
	}
	if s.RuntimeCheck(context.Background()) == nil {
		t.Fatal("runtime check must not pass while docker is unreachable")
	}
}

func TestRuntimePresent(t *testing.T) {
	s := newTestSupervisor(&fakeClient{runtimes: []string{"runc", "runsc"}}, "runsc", nil)
	if err := s.CheckStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.RuntimeCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestImageMissingFailsOnlyImageCheck(t *testing.T) {
	s := newTestSupervisor(&fakeClient{noImage: true}, "", nil)
	if err := s.CheckStartup(context.Background()); err != nil {
		t.Fatalf("missing image is a readiness problem, not a startup failure: %v", err)
	}
	ctx := context.Background()
	if s.DockerCheck(ctx) != nil || s.RuntimeCheck(ctx) != nil {
		t.Fatal("docker and runtime checks should pass")
	}
	if err := s.ImageCheck(ctx); err == nil || !strings.Contains(err.Error(), "nbsandbox-runner:dev") {
		t.Fatalf("image check: %v", err)
	}
}

func TestRecheckWakesLoop(t *testing.T) {
	fc := &fakeClient{}
	s := NewSupervisor(fc, SupervisorConfig{Image: "img", UpInterval: time.Hour, Logger: quietLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, "first check", func() bool { return fc.pings.Load() == 1 })
	s.Recheck()
	waitFor(t, "recheck", func() bool { return fc.pings.Load() == 2 })
}

func TestImageWithoutUsableManifestIsNotReady(t *testing.T) {
	for name, label := range map[string]string{"missing": "", "future format": `{"status_schema_version":3}`} {
		fc := &fakeClient{}
		s := newTestSupervisor(&labelClient{fakeClient: fc, label: label}, "", nil)
		_ = s.CheckStartup(context.Background())
		if err := s.ImageCheck(context.Background()); err == nil {
			t.Errorf("%s manifest: image check passed", name)
		}
		if s.Status().Manifest != nil {
			t.Errorf("%s manifest: manifest set", name)
		}
	}
}

type labelClient struct {
	*fakeClient
	label string
}

func (l *labelClient) ImageInspect(context.Context, string) (Image, error) {
	return Image{ID: "sha256:abc", Labels: map[string]string{"io.nbsandbox.manifest": l.label}}, nil
}
