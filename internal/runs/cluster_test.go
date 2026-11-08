// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

// node is one nbsandboxd process of a cluster: its own data folder, the
// shared database and object storage.
type node struct {
	m  *Manager
	sp *spaces.Service
	be *fakeBackend
}

func newNode(t *testing.T, db *store.DB, blobs blob.Store, id string, mod func(*Config)) *node {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil)).With("node", id)
	}
	sp, err := spaces.New(spaces.Config{DataDir: t.TempDir(), MaxFile: 1 << 20, MaxSpace: 10 << 20}, db, blobs, log)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Image: "nbsandbox-runner:test", Concurrency: 1, Timeout: 10 * time.Second, LogCap: 1 << 20,
		Retention: 24 * time.Hour, RetentionEvery: time.Hour, DependencyCheck: "warn",
		Limits:   isolation.Limits{MemoryBytes: 2 << 30, NanoCPUs: 1e9, PIDs: 256, TmpfsBytes: 256 << 20, StopGrace: 100 * time.Millisecond},
		WorkerID: id, SharedWorkers: true, Lease: 900 * time.Millisecond, QueuePoll: 100 * time.Millisecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	be := &fakeBackend{behave: succeed(10 * time.Millisecond)}
	d := Deps{DB: db, Spaces: sp, Backend: be, Log: log}
	if cfg.NoExecute {
		d.Available = func() bool { return true }
	}
	m := New(cfg, d)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.SetDockerUp(true)
	t.Cleanup(func() { m.Shutdown(0) })
	return &node{m: m, sp: sp, be: be}
}

type cluster struct {
	api    *node
	w1, w2 *node
	db     *store.DB
	space  string
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	db := storetest.Open(t)
	blobs := blob.NewMemory()
	c := &cluster{db: db}
	c.api = newNode(t, db, blobs, "api", func(c *Config) { c.NoExecute = true })
	c.w1 = newNode(t, db, blobs, "w1", nil)
	c.w2 = newNode(t, db, blobs, "w2", nil)
	sp, err := c.api.sp.Create(context.Background(), "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	c.space = sp.ID
	i := 0
	files := []string{"nb.ipynb", nb}
	if _, err := c.api.sp.Upload(context.Background(), "alice", sp.ID, "", func() (*spaces.UploadPart, error) {
		if i >= len(files) {
			return nil, io.EOF
		}
		p := &spaces.UploadPart{Name: files[i], Body: strings.NewReader(files[i+1])}
		i += 2
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *cluster) submit(t *testing.T, readOnly bool) View {
	t.Helper()
	v, _, err := c.api.m.Submit(context.Background(), "alice", c.space, SubmitRequest{Notebook: "nb.ipynb", ReadOnlySpace: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (c *cluster) wait(t *testing.T, id string, done bool) View {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := c.api.m.Get(context.Background(), "alice", c.space, id)
		if err != nil {
			t.Fatal(err)
		}
		if (done && terminal(v.Status)) || (!done && v.Status == StatusRunning) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stuck in %s", id, v.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An API node that executes nothing: workers take its runs; their logs,
// results and outputs are served by the API node, live and afterwards.
func TestClusterRunsOnWorkers(t *testing.T) {
	c := newCluster(t)
	c.w1.be.behave = lines(20, 15*time.Millisecond)
	c.w2.be.behave = lines(20, 15*time.Millisecond)
	ctx := context.Background()

	v := c.submit(t, false)
	ch, err := c.api.m.FollowLog(ctx, "alice", c.space, v.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, end := collectLog(t, ch)
	if len(got) != 21 || got[0] != "line 1" || got[19] != "line 20" || end.Kind != LogDone || end.Status != StatusSucceeded {
		t.Fatalf("followed on the API node: %d lines %q, end %+v", len(got), got, end)
	}
	if n := len(c.w1.be.startedRuns()) + len(c.w2.be.startedRuns()); n != 1 {
		t.Fatalf("started on %d workers", n)
	}
	res, err := c.api.m.GetResult(ctx, "alice", c.space, v.ID)
	if err != nil || res.Status != StatusSucceeded {
		t.Fatalf("result on the API node: %+v %v", res, err)
	}
	if f, _, err := c.api.m.OpenOutput(ctx, "alice", c.space, v.ID, "output.ipynb"); err != nil {
		t.Fatalf("output on the API node: %v", err)
	} else {
		_ = f.Close()
	}
	// The live log chunks went with the run.
	if chunks, _ := c.db.LogChunks(ctx, v.ID, 0); len(chunks) != 0 {
		t.Fatalf("%d live log chunks left", len(chunks))
	}

	// Read-only runs side by side: both workers take one.
	c.w1.be.behave = succeed(300 * time.Millisecond)
	c.w2.be.behave = succeed(300 * time.Millisecond)
	a, b := c.submit(t, true), c.submit(t, true)
	c.wait(t, a.ID, true)
	c.wait(t, b.ID, true)
	if len(c.w1.be.startedRuns()) == 0 || len(c.w2.be.startedRuns()) == 0 {
		t.Fatalf("one worker did everything: w1 %v, w2 %v", c.w1.be.startedRuns(), c.w2.be.startedRuns())
	}
}

// A cancel asked of the API node stops the run on its worker.
func TestClusterCancel(t *testing.T) {
	c := newCluster(t)
	c.w1.be.behave = succeed(time.Minute)
	c.w2.be.behave = succeed(time.Minute)
	v := c.submit(t, false)
	c.wait(t, v.ID, false)
	// The log so far, read on the API node while the run works elsewhere.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, _, err := c.api.m.OpenOutput(context.Background(), "alice", c.space, v.ID, "log.txt")
		if err == nil {
			b, _ := io.ReadAll(f)
			_ = f.Close()
			if strings.Contains(string(b), "hello from") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the running run's log isn't readable on the API node: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := c.api.m.Cancel(context.Background(), "alice", c.space, v.ID, "alice", "enough"); err != nil {
		t.Fatal(err)
	}
	if got := c.wait(t, v.ID, true); got.Status != StatusCancelled || got.CancelReason != "enough" {
		t.Fatalf("after cancel: %+v", got)
	}
}

// A worker that claims runs and goes away: a run it never started goes
// back to the queue and another worker runs it; a run it was running ends
// interrupted (retryable).
func TestClusterLeaseExpiry(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	// Keep the real workers away while the ghost claims.
	c.w1.m.SetDockerUp(false)
	c.w2.m.SetDockerUp(false)
	time.Sleep(50 * time.Millisecond)

	running := c.submit(t, true)
	r, err := c.db.ClaimRun(ctx, "ghost", 200*time.Millisecond)
	if err != nil || r.ID != running.ID {
		t.Fatalf("ghost claim: %+v %v", r, err)
	}
	if ok, _, err := c.db.MarkRunningWithEvent(ctx, r.ID, r.Token, time.Now(), nil); !ok || err != nil {
		t.Fatalf("ghost start: %v %v", ok, err)
	}
	queued := c.submit(t, true)
	if q, err := c.db.ClaimRun(ctx, "ghost", 200*time.Millisecond); err != nil || q.ID != queued.ID {
		t.Fatalf("ghost claim 2: %+v %v", q, err)
	}
	// A stale token can't start, finish or write anything.
	if ok, _, _ := c.db.MarkRunningWithEvent(ctx, queued.ID, r.Token, time.Now(), nil); ok {
		t.Fatal("started with another run's token")
	}

	c.w1.m.SetDockerUp(true)
	c.w2.m.SetDockerUp(true)
	if got := c.wait(t, running.ID, true); got.Status != StatusInterrupted || !got.Retryable {
		t.Fatalf("the ghost's running run: %+v", got)
	}
	if got := c.wait(t, queued.ID, true); got.Status != StatusSucceeded {
		t.Fatalf("the ghost's claimed run: %+v", got)
	}
	if ok, _, _ := c.db.FinishRunWithEvent(ctx, running.ID, store.Fence{Token: r.Token}, store.Outcome{Status: StatusSucceeded, FinishedAt: time.Now()}, nil); ok {
		t.Fatal("the ghost recorded an outcome after losing its lease")
	}
	if n, _ := c.db.QueueLength(ctx); n != 0 {
		t.Fatalf("queue length %d", n)
	}
}

// A worker whose lease is taken from it stops the run.
func TestClusterLostLease(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	c.w1.be.behave = succeed(time.Minute)
	c.w2.be.behave = succeed(time.Minute)
	v := c.submit(t, true)
	c.wait(t, v.ID, false)
	// Another node decides the run is lost (as if this worker had been
	// unreachable past its lease).
	r, _ := c.db.RunByID(ctx, v.ID)
	if ok, _, err := c.db.FinishRunWithEvent(ctx, v.ID, store.Fence{Token: r.Token},
		store.Outcome{Status: StatusInterrupted, Message: "taken over", FinishedAt: time.Now()}, nil); !ok || err != nil {
		t.Fatalf("take over: %v %v", ok, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.w1.m.ex.isActive(v.ID) || c.w2.m.ex.isActive(v.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the worker kept running a run it lost")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := c.wait(t, v.ID, true); got.Status != StatusInterrupted || got.Message != "taken over" {
		t.Fatalf("after: %+v", got)
	}
}

func TestRetryDB(t *testing.T) {
	tries := 0
	if err := retryDB(context.Background(), func() error {
		if tries++; tries < 3 {
			return errors.New("connection refused")
		}
		return nil
	}); err != nil || tries != 3 {
		t.Fatalf("%v after %d tries", err, tries)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tries = 0
	if err := retryDB(ctx, func() error { tries++; return errors.New("down") }); err == nil || tries != 1 {
		t.Fatalf("cancelled: %v after %d tries", err, tries)
	}
}
