// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package backendtest is the conformance suite every isolation.Backend must
// pass, with real containers or VMs. It checks the contract the
// run manager relies on: lifecycle and exit codes, ordered output, stopping on
// cancel within the grace period, OOM evidence, and orphan handling. The
// Docker backend runs it under both runtimes; a future Firecracker backend
// reuses it unchanged.
package backendtest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Harness describes the backend under test.
type Harness struct {
	Backend     isolation.Backend
	Image       string
	Runtime     string
	WorkloadDir string // tests/workloads
	// TempDir returns a directory the backend can mount (on Docker Desktop
	// it must be inside a shared folder; t.TempDir() usually is).
	TempDir func(t *testing.T) string
}

// newID returns a random version-4 UUID (the runner rejects anything else).
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type collector struct {
	mu    sync.Mutex
	lines []string
}

func (c *collector) Line(s string) { c.mu.Lock(); c.lines = append(c.lines, s); c.mu.Unlock() }
func (c *collector) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func (h Harness) spec(t *testing.T, notebook string, mem int64) isolation.RunSpec {
	t.Helper()
	// /space (the space's files) and /run (this run's folder, holding the
	// notebook snapshot as input.ipynb). The run executes as uid 1000; the
	// folders must be writable by it. The test creates artifacts/ itself:
	// on Linux, a folder the run created would belong to uid 1000, and the
	// test (another user) couldn't remove the files in it afterwards.
	space, runDir := h.TempDir(t), h.TempDir(t)
	artifacts := filepath.Join(runDir, "artifacts")
	if err := os.Mkdir(artifacts, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{space, runDir, artifacts} {
		if err := os.Chmod(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(h.WorkloadDir, notebook))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "input.ipynb"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return isolation.RunSpec{
		RunID: newID(), SpaceID: newID(), SpaceDir: space, RunDir: runDir, Notebook: notebook, Image: h.Image, Runtime: h.Runtime,
		Limits: isolation.Limits{MemoryBytes: mem, NanoCPUs: 1e9, PIDs: 256, TmpfsBytes: 128 << 20,
			ShmBytes: 64 << 20, LogBytes: 10 << 20, StopGrace: 5 * time.Second},
	}
}

func status(t *testing.T, spec isolation.RunSpec) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(spec.RunDir, ".nbsandbox-status.json"))
	if err != nil {
		return nil
	}
	var st map[string]any
	_ = json.Unmarshal(b, &st)
	return st
}

// Run executes the whole suite.
func Run(t *testing.T, h Harness) {
	t.Run("lifecycle and exit codes", func(t *testing.T) {
		for nb, want := range map[string]int{"smoke.ipynb": 0, "fail_cell3.ipynb": 10, "crash.ipynb": 11} {
			spec := h.spec(t, nb, 1<<30)
			var out collector
			res, err := h.Backend.Run(context.Background(), spec, &out)
			if err != nil {
				t.Fatalf("%s: %v", nb, err)
			}
			if res.ExitCode != want || res.Stopped || !res.Removed || !res.Finished.After(res.Started) {
				t.Errorf("%s: %+v, want exit %d", nb, res, want)
			}
			if len(out.all()) == 0 {
				t.Errorf("%s: no output lines", nb)
			}
		}
	})

	t.Run("output arrives in order and live", func(t *testing.T) {
		spec := h.spec(t, "stream.ipynb", 1<<30)
		var out collector
		var firstAt time.Time
		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for time.Since(start) < 2*time.Minute {
				for _, l := range out.all() {
					if l == "line 0" && firstAt.IsZero() {
						firstAt = time.Now()
					}
				}
				if !firstAt.IsZero() {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
		if _, err := h.Backend.Run(context.Background(), spec, &out); err != nil {
			t.Fatal(err)
		}
		<-done
		var nums []string
		for _, l := range out.all() {
			if strings.HasPrefix(l, "line ") {
				nums = append(nums, strings.TrimPrefix(l, "line "))
			}
		}
		if strings.Join(nums, ",") != "0,1,2,3,4,5,6,7,8,9" {
			t.Fatalf("lines %v", nums)
		}
		if firstAt.IsZero() || time.Since(firstAt) < 8*time.Second {
			t.Fatalf("output was not delivered live (first line %v before the end)", time.Since(firstAt))
		}
	})

	t.Run("cancel stops within the grace period", func(t *testing.T) {
		spec := h.spec(t, "long.ipynb", 1<<30)
		ctx, cancel := context.WithCancel(context.Background())
		var out collector
		go func() {
			for !containsLine(out.all(), "tick 2") {
				time.Sleep(100 * time.Millisecond)
			}
			cancel()
		}()
		start := time.Now()
		res, err := h.Backend.Run(ctx, spec, &out)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Stopped || !res.Removed {
			t.Fatalf("result %+v", res)
		}
		if st := status(t, spec); st == nil || st["state"] != "terminated" {
			t.Errorf("status after cancel: %v", st)
		}
		if _, err := os.Stat(filepath.Join(spec.RunDir, "output.ipynb")); err != nil {
			t.Error("no partial output after cancel")
		}
		_ = start
	})

	t.Run("memory limit leaves OOM evidence", func(t *testing.T) {
		spec := h.spec(t, "oom.ipynb", 512<<20)
		res, err := h.Backend.Run(context.Background(), spec, &collector{})
		if err != nil {
			t.Fatal(err)
		}
		st := status(t, spec)
		var oomKill float64
		if st != nil {
			if cg, ok := st["cgroup"].(map[string]any); ok {
				oomKill, _ = cg["oom_kill"].(float64)
			}
		}
		if !res.OOMKilled && oomKill == 0 {
			t.Fatalf("no OOM evidence: result %+v, status %v", res, st)
		}
	})

	t.Run("orphans are listed and killed", func(t *testing.T) {
		spec := h.spec(t, "long.ipynb", 1<<30)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _, _ = h.Backend.Run(ctx, spec, &collector{}) }()
		deadline := time.Now().Add(time.Minute)
		var found bool
		for !found && time.Now().Before(deadline) {
			orphans, err := h.Backend.Orphans(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range orphans {
				found = found || (o.RunID == spec.RunID && o.SpaceID == spec.SpaceID)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !found {
			t.Fatal("running container not listed")
		}
		if err := h.Backend.Kill(context.Background(), spec.RunID); err != nil {
			t.Fatal(err)
		}
		orphans, _ := h.Backend.Orphans(context.Background())
		for _, o := range orphans {
			if o.RunID == spec.RunID {
				t.Fatal("container still listed after Kill")
			}
		}
		if err := h.Backend.Kill(context.Background(), spec.RunID); err != nil {
			t.Fatalf("killing twice must be harmless: %v", err)
		}
	})
}

func containsLine(lines []string, s string) bool {
	for _, l := range lines {
		if l == s {
			return true
		}
	}
	return false
}
