// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

const nb = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": [
 {"cell_type": "code", "source": "import pandas\nimport torch", "metadata": {}, "outputs": []}]}`

// fakeBackend plays the runner: behave decides what each run does.
type fakeBackend struct {
	mu      sync.Mutex
	behave  func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error)
	started []string
	killed  []string
	orphans []isolation.Orphan
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Run(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
	f.mu.Lock()
	f.started = append(f.started, spec.RunID)
	b := f.behave
	f.mu.Unlock()
	return b(ctx, spec, out)
}
func (f *fakeBackend) killedRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

func (f *fakeBackend) Orphans(context.Context) ([]isolation.Orphan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orphans, nil
}
func (f *fakeBackend) Kill(_ context.Context, runID string) error {
	f.mu.Lock()
	f.killed = append(f.killed, runID)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) startedRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

// writeStatus does what the wrapper does inside the container.
func writeStatus(spec isolation.RunSpec, state string, extra map[string]any) {
	st := map[string]any{"schema_version": 2, "state": state, "rendered": true, "finished_at": "x",
		"cgroup": map[string]any{"memory_peak_bytes": 150 << 20, "cpu_usage_usec": 2_500_000}}
	for k, v := range extra {
		st[k] = v
	}
	b, _ := json.Marshal(st)
	dir := spec.RunDir
	_ = os.WriteFile(filepath.Join(dir, ".nbsandbox-status.json"), b, 0o600)
}

func writeOutputNotebook(spec isolation.RunSpec) {
	_ = os.WriteFile(filepath.Join(spec.RunDir, "output.ipynb"), []byte(nb), 0o600)
}

// succeed is a run that prints lines and succeeds after d.
func succeed(d time.Duration) func(context.Context, isolation.RunSpec, isolation.LineSink) (isolation.RunResult, error) {
	return func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		out.Line("hello from " + spec.Notebook)
		select {
		case <-time.After(d):
			writeOutputNotebook(spec)
			writeStatus(spec, "succeeded", nil)
			return isolation.RunResult{ExitCode: 0}, nil
		case <-ctx.Done():
			writeOutputNotebook(spec)
			writeStatus(spec, "terminated", nil)
			return isolation.RunResult{ExitCode: 143, Stopped: true}, nil
		}
	}
}

type env struct {
	m       *Manager
	be      *fakeBackend
	db      *store.DB
	sp      *spaces.Service
	metrics *obs.Metrics
	now     *atomic.Int64
	cfg     Config
}

func newEnv(t *testing.T, mod ...func(*Config)) *env {
	t.Helper()
	data := t.TempDir()
	db := storetest.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp, err := spaces.New(spaces.Config{DataDir: data, MaxFile: 1 << 20, MaxSpace: 10 << 20}, db, blob.NewMemory(), log)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Image: "nbsandbox-runner:test", Concurrency: 2, Timeout: 5 * time.Second, LogCap: 1 << 20,
		Retention: 24 * time.Hour, RetentionEvery: time.Hour, DependencyCheck: "warn",
		Limits: isolation.Limits{MemoryBytes: 2 << 30, NanoCPUs: 1e9, PIDs: 256, TmpfsBytes: 256 << 20, StopGrace: 100 * time.Millisecond},
	}
	for _, f := range mod {
		f(&cfg)
	}
	be := &fakeBackend{behave: succeed(10 * time.Millisecond)}
	manifest, err := deps.ParseManifest(`{"status_schema_version":2,"distributions":{"pandas":"3"},"import_names":{"pandas":["pandas"]},"stdlib":["os"]}`)
	if err != nil {
		t.Fatal(err)
	}
	var now atomic.Int64
	now.Store(time.Now().UnixMilli())
	metrics := obs.NewMetrics()
	m := New(cfg, Deps{DB: db, Spaces: sp, Backend: be, Manifest: func() *deps.Manifest { return manifest },
		ImageID: func() string { return "sha256:img" }, Metrics: metrics, Log: log,
		Now: func() time.Time { return time.UnixMilli(now.Load()) }})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.SetDockerUp(true)
	t.Cleanup(func() { m.Shutdown(0) })
	return &env{m: m, be: be, db: db, sp: sp, metrics: metrics, now: &now, cfg: cfg}
}

func (e *env) space(t *testing.T, owner string) string {
	t.Helper()
	s, err := e.sp.Create(context.Background(), owner, "")
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	files := []string{"nb.ipynb", nb}
	if _, err := e.sp.Upload(context.Background(), owner, s.ID, "", func() (*spaces.UploadPart, error) {
		if i >= len(files) {
			return nil, io.EOF
		}
		p := &spaces.UploadPart{Name: files[i], Body: strings.NewReader(files[i+1])}
		i += 2
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

func (e *env) submit(t *testing.T, owner, spaceID string, params string) View {
	t.Helper()
	v, _, err := e.m.Submit(context.Background(), owner, spaceID, SubmitRequest{Notebook: "nb.ipynb", Parameters: json.RawMessage(params)})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) wait(t *testing.T, owner, spaceID, runID string, terminal bool) View {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := e.m.Get(context.Background(), owner, spaceID, runID)
		if err != nil {
			t.Fatal(err)
		}
		if (terminal && v.Status != StatusQueued && v.Status != StatusRunning) || (!terminal && v.Status == StatusRunning) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stuck in %s", runID, v.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunSucceeds(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	v := e.submit(t, "alice", sid, `{"region": "jakarta", "months": 6}`)
	if v.Status != StatusQueued && v.Status != StatusRunning {
		t.Fatalf("submitted status %s", v.Status)
	}
	// Torch isn't in the manifest.
	if len(v.DependencyWarnings) != 1 || v.DependencyWarnings[0].Module != "torch" {
		t.Fatalf("warnings %+v", v.DependencyWarnings)
	}
	v = e.wait(t, "alice", sid, v.ID, true)
	if v.Status != StatusSucceeded || v.Result == nil {
		t.Fatalf("view %+v", v)
	}
	r := v.Result
	if r.Status != StatusSucceeded || !r.Rendered || r.Partial || r.Image.ID != "sha256:img" || r.Runtime != "runc" ||
		*r.Usage.PeakMemoryBytes != 150<<20 || *r.Usage.CPUSeconds != 2.5 || r.Limits.MemoryBytes != 2<<30 ||
		compact(r.Parameters) != `{"months":6,"region":"jakarta"}` || len(r.DependencyWarnings) != 1 {
		t.Fatalf("result %+v", r)
	}
	f, _, err := e.m.OpenOutput(context.Background(), "alice", sid, v.ID, spaces.RunLog)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "hello from nb.ipynb\n[nbsandbox] run finished: succeeded\n" {
		t.Fatalf("log %q", b)
	}
	params, _ := e.sp.ReadRunFile(context.Background(), sid, v.ID, spaces.RunParams, 1024)
	if string(params) != `{"months":6,"region":"jakarta"}` {
		t.Fatalf("params file %q", params)
	}
	// Ownership: bob sees nothing.
	if _, err := e.m.Get(context.Background(), "bob", sid, v.ID); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("bob Get: %v", err)
	}
	if _, _, err := e.m.OpenOutput(context.Background(), "bob", sid, v.ID, spaces.RunLog); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatal("bob reads log")
	}
}

func TestSubmitValidation(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	var ie *spaces.InvalidError
	var pe *spaces.PathError
	cases := map[string]SubmitRequest{
		"missing notebook":     {Notebook: "nope.ipynb"},
		"not a notebook":       {Notebook: "nb.txt"},
		"traversal":            {Notebook: "../x.ipynb"},
		"params not object":    {Notebook: "nb.ipynb", Parameters: json.RawMessage(`[1]`)},
		"params too deep":      {Notebook: "nb.ipynb", Parameters: json.RawMessage(`{"a": ` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}`)},
		"params bad name":      {Notebook: "nb.ipynb", Parameters: json.RawMessage(`{"not-python": 1}`)},
		"params keyword":       {Notebook: "nb.ipynb", Parameters: json.RawMessage(`{"class": 1}`)},
		"params too large":     {Notebook: "nb.ipynb", Parameters: json.RawMessage(`{"a": "` + strings.Repeat("x", DefaultMaxParamsBytes) + `"}`)},
		"idempotency key long": {Notebook: "nb.ipynb", IdempotencyKey: strings.Repeat("k", 256)},
	}
	for name, req := range cases {
		if _, _, err := e.m.Submit(context.Background(), "alice", sid, req); !errors.As(err, &ie) && !errors.As(err, &pe) {
			t.Errorf("%s: %v, want InvalidError or PathError (both 400)", name, err)
		}
	}
	if _, _, err := e.m.Submit(context.Background(), "bob", sid, SubmitRequest{Notebook: "nb.ipynb"}); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("bob submits: %v", err)
	}
	if got := e.be.startedRuns(); len(got) != 0 {
		t.Fatalf("rejected requests consumed a run slot: %v", got)
	}
	// Any JSON value is accepted under a valid name: nested objects, arrays
	// of objects, null, big and precise numbers, unicode. Keys are sorted at
	// every level, numbers kept exactly as sent.
	ok := `{"s": "x", "n": 1.5, "i": -3, "b": true, "l": ["a", 1, false], "z": null,
		"cfg": {"months": 6, "include": ["sales", {"kind": "returns", "w": 0.25}], "deep": [[[1]]]},
		"big": 123456789012345678901234567890, "u": "jakarta ✓"}`
	got, err := NormalizeParameters(json.RawMessage(ok), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"b":true,"big":123456789012345678901234567890,"cfg":{"deep":[[[1]]],"include":["sales",{"kind":"returns","w":0.25}],"months":6},` +
		`"i":-3,"l":["a",1,false],"n":1.5,"s":"x","u":"jakarta ✓","z":null}`
	if got != want {
		t.Fatalf("canonical form:\n got %s\nwant %s", got, want)
	}
	if _, err := NormalizeParameters(json.RawMessage(`{"a": "`+strings.Repeat("x", 2000)+`"}`), 1024); err == nil {
		t.Fatal("configured size limit not applied")
	}
}

func TestDependencyRejectMode(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.DependencyCheck = "reject" })
	sid := e.space(t, "alice")
	_, _, err := e.m.Submit(context.Background(), "alice", sid, SubmitRequest{Notebook: "nb.ipynb"})
	var de *DependencyError
	if !errors.As(err, &de) || de.Warnings[0].Module != "torch" {
		t.Fatalf("got %v", err)
	}
}

func TestOneActiveRunPerSpaceAndIdempotency(t *testing.T) {
	e := newEnv(t)
	e.be.behave = succeed(300 * time.Millisecond)
	sid := e.space(t, "alice")
	ctx := context.Background()
	first, replay, err := e.m.Submit(ctx, "alice", sid, SubmitRequest{Notebook: "nb.ipynb", IdempotencyKey: "k1"})
	if err != nil || replay {
		t.Fatal(err)
	}
	if _, _, err := e.m.Submit(ctx, "alice", sid, SubmitRequest{Notebook: "nb.ipynb"}); !errors.Is(err, ErrSpaceBusy) {
		t.Fatalf("second active run: %v", err)
	}
	again, replay, err := e.m.Submit(ctx, "alice", sid, SubmitRequest{Notebook: "nb.ipynb", IdempotencyKey: "k1"})
	if err != nil || !replay || again.ID != first.ID {
		t.Fatalf("replay: %v %v %v", again.ID, replay, err)
	}
	if _, _, err := e.m.Submit(ctx, "alice", sid, SubmitRequest{Notebook: "nb.ipynb", IdempotencyKey: "k1",
		Parameters: json.RawMessage(`{"x": 1}`)}); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	e.wait(t, "alice", sid, first.ID, true)
	if n := len(e.be.startedRuns()); n != 1 {
		t.Fatalf("started %d runs, want 1", n)
	}
}

// Global limit 2, FIFO queue, positions.
func TestQueueConcurrencyAndOrder(t *testing.T) {
	e := newEnv(t)
	// Each run waits for its own release, so slots free one at a time: with
	// a single idle worker the next run to start must be the oldest queued
	// one, whatever the timing.
	var relMu sync.Mutex
	releases := map[string]chan struct{}{}
	releaseOf := func(id string) chan struct{} {
		relMu.Lock()
		defer relMu.Unlock()
		if releases[id] == nil {
			releases[id] = make(chan struct{})
		}
		return releases[id]
	}
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		<-releaseOf(spec.RunID)
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
	var ids, sids []string
	for i := 0; i < 4; i++ {
		sid := e.space(t, "alice")
		sids = append(sids, sid)
		ids = append(ids, e.submit(t, "alice", sid, "").ID)
	}
	e.wait(t, "alice", sids[0], ids[0], false)
	e.wait(t, "alice", sids[1], ids[1], false)
	time.Sleep(50 * time.Millisecond)
	for i, want := range map[int]int{2: 1, 3: 2} {
		v, _ := e.m.Get(context.Background(), "alice", sids[i], ids[i])
		if v.Status != StatusQueued || v.QueuePosition != want {
			t.Fatalf("run %d: %s position %d, want queued %d", i, v.Status, v.QueuePosition, want)
		}
	}
	if n := len(e.be.startedRuns()); n != 2 {
		t.Fatalf("%d runs started with concurrency 2", n)
	}
	// Free one slot: the oldest queued run (2) must start, not 3.
	close(releaseOf(ids[0]))
	e.wait(t, "alice", sids[2], ids[2], false)
	// "running" comes before the backend starts it (the space is checked
	// out in between): wait for the start itself.
	for deadline := time.Now().Add(5 * time.Second); len(e.be.startedRuns()) < 3 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.be.startedRuns(); len(got) != 3 || got[2] != ids[2] {
		t.Fatalf("after one slot freed, started %v; want run 2 third", got)
	}
	if v, _ := e.m.Get(context.Background(), "alice", sids[3], ids[3]); v.Status != StatusQueued || v.QueuePosition != 1 {
		t.Fatalf("run 3: %s position %d, want queued 1", v.Status, v.QueuePosition)
	}
	close(releaseOf(ids[1]))
	close(releaseOf(ids[2]))
	close(releaseOf(ids[3]))
	for i := range ids {
		if v := e.wait(t, "alice", sids[i], ids[i], true); v.Status != StatusSucceeded {
			t.Fatalf("run %d: %s", i, v.Status)
		}
	}
	if got := e.be.startedRuns(); !slices.Equal(got, ids) {
		t.Fatalf("queue not FIFO: started %v, submitted %v", got, ids)
	}
}

// Cancel a running run.
func TestCancelRunning(t *testing.T) {
	e := newEnv(t)
	e.be.behave = succeed(time.Hour)
	sid := e.space(t, "alice")
	v := e.submit(t, "alice", sid, "")
	e.wait(t, "alice", sid, v.ID, false)
	if _, err := e.m.Cancel(context.Background(), "alice", sid, v.ID, "alice", ""); err != nil {
		t.Fatal(err)
	}
	v = e.wait(t, "alice", sid, v.ID, true)
	if v.Status != StatusCancelled || v.Fault != FaultCaller || v.Result == nil || !v.Result.Partial {
		t.Fatalf("view %+v result %+v", v, v.Result)
	}
	if _, err := e.m.Cancel(context.Background(), "alice", sid, v.ID, "alice", ""); !errors.Is(err, ErrAlreadyFinished) {
		t.Fatalf("cancel twice: %v", err)
	}
}

// A cancelled queued run never starts.
func TestCancelQueued(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Concurrency = 1 })
	e.be.behave = succeed(200 * time.Millisecond)
	busy := e.space(t, "alice")
	first := e.submit(t, "alice", busy, "")
	e.wait(t, "alice", busy, first.ID, false)
	sid := e.space(t, "alice")
	v := e.submit(t, "alice", sid, "")
	if v.QueuePosition != 1 {
		t.Fatalf("position %d", v.QueuePosition)
	}
	v, err := e.m.Cancel(context.Background(), "alice", sid, v.ID, "alice", "")
	if err != nil || v.Status != StatusCancelled || v.Result == nil || v.Result.Partial {
		t.Fatalf("cancel queued: %+v %v", v, err)
	}
	e.wait(t, "alice", busy, first.ID, true)
	time.Sleep(50 * time.Millisecond)
	for _, id := range e.be.startedRuns() {
		if id == v.ID {
			t.Fatal("cancelled queued run was started")
		}
	}
}

func TestTimeout(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Timeout = 100 * time.Millisecond })
	e.be.behave = succeed(time.Hour)
	sid := e.space(t, "alice")
	v := e.wait(t, "alice", sid, e.submit(t, "alice", sid, "").ID, true)
	if v.Status != StatusTimeout || v.Fault != FaultNotebook || !v.Result.Partial {
		t.Fatalf("view %+v", v)
	}
}

func TestPlatformFailuresAreNotNotebookFaults(t *testing.T) {
	cases := map[string]struct {
		err    error
		status string
	}{
		"start failed": {fmt.Errorf("%w: no such image", isolation.ErrStart), StatusPlatformError},
		"docker lost":  {fmt.Errorf("%w: EOF", isolation.ErrDockerLost), StatusInterrupted},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.be.behave = func(context.Context, isolation.RunSpec, isolation.LineSink) (isolation.RunResult, error) {
				return isolation.RunResult{}, c.err
			}
			sid := e.space(t, "alice")
			v := e.wait(t, "alice", sid, e.submit(t, "alice", sid, "").ID, true)
			if v.Status != c.status || v.Fault != FaultPlatform {
				t.Fatalf("view %+v", v)
			}
			if c.status == StatusInterrupted && len(e.be.killedRuns()) == 0 {
				t.Fatal("container not cleaned up after Docker loss")
			}
		})
	}
}

// Cancel racing completion leaves
// exactly one terminal status, the same in the database and result.json.
func TestCancelCompletionRace(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		finish := make(chan struct{})
		e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
			select {
			case <-finish:
				writeStatus(spec, "succeeded", nil)
				return isolation.RunResult{}, nil
			case <-ctx.Done():
				writeStatus(spec, "terminated", nil)
				return isolation.RunResult{Stopped: true, ExitCode: 143}, nil
			}
		}
		v := e.submit(t, "alice", sid, "")
		e.wait(t, "alice", sid, v.ID, false)
		// Random timing on both sides so either can win.
		go func() {
			time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
			close(finish)
		}()
		time.Sleep(time.Duration(rand.IntN(1000)) * time.Microsecond)
		_, _ = e.m.Cancel(context.Background(), "alice", sid, v.ID, "alice", "")
		v = e.wait(t, "alice", sid, v.ID, true)
		if v.Result == nil || v.Result.Status != v.Status {
			t.Fatalf("round %d: db %s, result.json %+v", i, v.Status, v.Result)
		}
		if v.Status != StatusSucceeded && v.Status != StatusCancelled {
			t.Fatalf("round %d: unexpected %s", i, v.Status)
		}
		counts[v.Status]++
	}
	// Which side wins the random rounds depends on load (under heavy load
	// the cancel can lose every time), so they only check consistency. Each
	// outcome is forced once below, so both paths always run.
	t.Logf("outcomes over 200 random rounds: %v", counts)

	for _, cancelFirst := range []bool{true, false} {
		finish := make(chan struct{})
		e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
			select {
			case <-finish:
				writeStatus(spec, "succeeded", nil)
				return isolation.RunResult{}, nil
			case <-ctx.Done():
				writeStatus(spec, "terminated", nil)
				return isolation.RunResult{Stopped: true, ExitCode: 143}, nil
			}
		}
		v := e.submit(t, "alice", sid, "")
		e.wait(t, "alice", sid, v.ID, false)
		want := StatusCancelled
		if cancelFirst {
			if _, err := e.m.Cancel(context.Background(), "alice", sid, v.ID, "alice", ""); err != nil {
				t.Fatal(err)
			}
			close(finish)
		} else {
			want = StatusSucceeded
			close(finish)
			e.wait(t, "alice", sid, v.ID, true)
			if _, err := e.m.Cancel(context.Background(), "alice", sid, v.ID, "alice", ""); !errors.Is(err, ErrAlreadyFinished) {
				t.Fatalf("cancel after the end: %v", err)
			}
		}
		v = e.wait(t, "alice", sid, v.ID, true)
		if v.Status != want || v.Result == nil || v.Result.Status != want {
			t.Fatalf("cancel first %v: db %s, result.json %+v, want %s", cancelFirst, v.Status, v.Result, want)
		}
	}
}

// The caller going away doesn't affect the run.
func TestCallerDisconnect(t *testing.T) {
	e := newEnv(t)
	e.be.behave = succeed(100 * time.Millisecond)
	sid := e.space(t, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	v, _, err := e.m.Submit(ctx, "alice", sid, SubmitRequest{Notebook: "nb.ipynb"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if v := e.wait(t, "alice", sid, v.ID, true); v.Status != StatusSucceeded {
		t.Fatalf("status %s", v.Status)
	}
}

// Nothing starts while Docker is down; submissions get 503.
func TestDockerDown(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	e.m.SetDockerUp(false)
	if _, _, err := e.m.Submit(context.Background(), "alice", sid, SubmitRequest{Notebook: "nb.ipynb"}); !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("submit while down: %v", err)
	}
}

// Leftovers after a restart are interrupted and removed.
func TestReconcile(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.NoExecute = true }) // no worker to claim the planted run
	sid := e.space(t, "alice")
	ctx := context.Background()
	id := spaces.NewID()
	_, _, _ = e.db.SubmitRun(ctx, store.NewRun{ID: id, SpaceID: sid, Owner: "alice", Notebook: "nb.ipynb", CreatedAt: time.Now()}, nil)
	_, _ = e.db.MarkRunning(ctx, id, time.Now())
	e.be.mu.Lock()
	e.be.orphans = []isolation.Orphan{{RunID: id, SpaceID: sid, Running: true}, {RunID: "unknown-run", Running: false}}
	e.be.mu.Unlock()

	e.m.Reconcile(ctx)
	v, _ := e.m.Get(ctx, "alice", sid, id)
	if v.Status != StatusInterrupted || v.Fault != FaultPlatform || v.Result == nil {
		t.Fatalf("after reconcile: %+v", v)
	}
	if strings.Join(e.be.killedRuns(), ",") != id+",unknown-run" {
		t.Fatalf("killed %v", e.be.killedRuns())
	}
}

// Shutdown interrupts active runs and keeps
// queued ones queued.
func TestShutdown(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Concurrency = 1 })
	e.be.behave = succeed(time.Hour)
	a, b := e.space(t, "alice"), e.space(t, "alice")
	running := e.submit(t, "alice", a, "")
	e.wait(t, "alice", a, running.ID, false)
	queued := e.submit(t, "alice", b, "")

	e.m.Shutdown(50 * time.Millisecond)
	ctx := context.Background()
	if r, _ := e.db.RunByID(ctx, running.ID); r.Status != StatusInterrupted || r.ErrorKind != KindShutdown {
		t.Fatalf("running run after shutdown: %s/%s", r.Status, r.ErrorKind)
	}
	if r, _ := e.db.RunByID(ctx, queued.ID); r.Status != StatusQueued {
		t.Fatalf("queued run after shutdown: %s", r.Status)
	}
	if _, _, err := e.m.Submit(ctx, "alice", e.space(t, "alice"), SubmitRequest{Notebook: "nb.ipynb"}); !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("submit while shutting down: %v", err)
	}
}

// Retention deletes old runs and updates usage.
func TestRetention(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	v := e.wait(t, "alice", sid, e.submit(t, "alice", sid, "").ID, true)
	ctx := context.Background()
	before, _ := e.sp.Usage(ctx, sid)
	if before.RunCount != 1 {
		t.Fatalf("usage %+v", before)
	}
	e.m.RunRetention(ctx) // too recent: kept
	if _, err := e.m.Get(ctx, "alice", sid, v.ID); err != nil {
		t.Fatal("recent run deleted")
	}
	e.now.Add((25 * time.Hour).Milliseconds())
	e.m.RunRetention(ctx)
	if _, err := e.m.Get(ctx, "alice", sid, v.ID); !errors.Is(err, spaces.ErrNotFound) {
		t.Fatalf("old run still there: %v", err)
	}
	after, _ := e.sp.Usage(ctx, sid)
	if after.RunCount != 0 || after.BytesUsed >= before.BytesUsed {
		t.Fatalf("usage not updated: before %+v after %+v", before, after)
	}
}

func TestDeleteRun(t *testing.T) {
	e := newEnv(t)
	e.be.behave = succeed(200 * time.Millisecond)
	sid := e.space(t, "alice")
	v := e.submit(t, "alice", sid, "")
	if err := e.m.Delete(context.Background(), "alice", sid, v.ID); !errors.Is(err, ErrNotFinished) {
		t.Fatalf("delete active: %v", err)
	}
	e.wait(t, "alice", sid, v.ID, true)
	if err := e.m.Delete(context.Background(), "alice", sid, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.sp.Dir(sid), "runs", v.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("run dir not removed")
	}
	if !e.m.Active(context.Background(), sid) == false {
		t.Fatal("space still active")
	}
}

func TestListRunsAndMetrics(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	for i := 0; i < 3; i++ {
		e.wait(t, "alice", sid, e.submit(t, "alice", sid, "").ID, true)
	}
	list, next, err := e.m.List(context.Background(), "alice", sid, ListFilter{Status: StatusSucceeded}, "", 2)
	if err != nil || len(list) != 2 || next == "" || list[0].Result != nil {
		t.Fatalf("list %d %q %v", len(list), next, err)
	}
	if _, _, err := e.m.List(context.Background(), "alice", sid, ListFilter{}, "bad!", 2); err == nil {
		t.Fatal("bad cursor accepted")
	}
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	_ = json.Compact(&b, raw)
	return b.String()
}
