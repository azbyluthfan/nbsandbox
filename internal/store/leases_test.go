// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestClaimsAndLeases(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	if _, err := db.ClaimRun(ctx, "w1", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim from an empty queue: %v", err)
	}
	for _, id := range []string{"r1", "r2", "r3"} {
		if _, _, _, err := db.SubmitRunWithEvent(ctx, NewRun{ID: id, SpaceID: "s1", Owner: "alice", Notebook: "nb.ipynb",
			CreatedAt: time.Now(), ReadOnlySpace: true}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := db.QueueLength(ctx); n != 3 {
		t.Fatalf("queue length %d", n)
	}
	if p, _ := db.QueuePosition(ctx, "r2"); p != 2 {
		t.Fatalf("r2 position %d", p)
	}

	// Concurrent claims never take the same run.
	var mu sync.Mutex
	var claimed []Run
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := db.ClaimRun(ctx, "w1", time.Minute); err == nil {
				mu.Lock()
				claimed = append(claimed, r)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != 3 {
		t.Fatalf("claimed %d runs", len(claimed))
	}
	slices.SortFunc(claimed, func(a, b Run) int { return int(a.QueueSeq - b.QueueSeq) })
	r1, r2, r3 := claimed[0], claimed[1], claimed[2]
	if r1.ID != "r1" || r1.Worker != "w1" || r1.Token == 0 || r1.Token == r2.Token || !r1.LeaseUntil.After(time.Now()) {
		t.Fatalf("claim %+v", r1)
	}
	// Claimed runs are still queued: the position counts them.
	if p, _ := db.QueuePosition(ctx, "r3"); p != 3 {
		t.Fatalf("r3 position %d", p)
	}

	// Only the token's holder starts a run.
	if ok, _, _ := db.MarkRunningWithEvent(ctx, "r1", r2.Token, time.Now(), nil); ok {
		t.Fatal("started with another token")
	}
	if ok, _, err := db.MarkRunningWithEvent(ctx, "r1", r1.Token, time.Now(), nil); !ok || err != nil {
		t.Fatalf("start: %v %v", ok, err)
	}

	// Renewing: a stale token is reported lost.
	lost, err := db.RenewLeases(ctx, []Lease{{"r1", r1.Token}, {"r2", r1.Token}}, time.Minute)
	if err != nil || !slices.Equal(lost, []string{"r2"}) {
		t.Fatalf("lost %v %v", lost, err)
	}
	if lost, err := db.RenewLeases(ctx, nil, time.Minute); lost != nil || err != nil {
		t.Fatal("renewing nothing")
	}

	// A claim that never started goes back to the queue, with its token.
	if ok, _ := db.ReleaseRun(ctx, "r2", r1.Token); ok {
		t.Fatal("released with another token")
	}
	if ok, err := db.ReleaseRun(ctx, "r2", r2.Token); !ok || err != nil {
		t.Fatalf("release: %v %v", ok, err)
	}
	again, err := db.ClaimRun(ctx, "w2", time.Minute)
	if err != nil || again.ID != "r2" || again.Token == r2.Token {
		t.Fatalf("reclaim %+v %v", again, err)
	}

	// Expired leases (a negative lease is already over).
	if _, err := db.RenewLeases(ctx, []Lease{{"r3", r3.Token}}, -time.Second); err != nil {
		t.Fatal(err)
	}
	exp, err := db.ExpiredLeases(ctx)
	if err != nil || len(exp) != 1 || exp[0].ID != "r3" {
		t.Fatalf("expired %+v %v", exp, err)
	}

	// Cancel requests.
	if _, err := db.RequestCancel(ctx, "r1", "alice", "stop"); err != nil {
		t.Fatal(err)
	}
	if c, err := db.CancelRequested(ctx, []string{"r1", "r2"}); err != nil || !slices.Equal(c, []string{"r1"}) {
		t.Fatalf("cancel requested %v %v", c, err)
	}
	if c, _ := db.CancelRequested(ctx, nil); c != nil {
		t.Fatal("cancel requested of nothing")
	}

	// Fenced finishes.
	done := Outcome{Status: "succeeded", FinishedAt: time.Now()}
	if ok, _, _ := db.FinishRunWithEvent(ctx, "r1", Fence{Token: r2.Token}, done, nil); ok {
		t.Fatal("finished with another token")
	}
	if ok, _, _ := db.FinishRunWithEvent(ctx, "r1", Fence{QueuedOnly: true}, done, nil); ok {
		t.Fatal("finished a running run as queued-only")
	}
	if ok, _, _ := db.FinishRunWithEvent(ctx, "r3", Fence{QueuedOnly: true}, Outcome{Status: "cancelled", FinishedAt: time.Now()}, nil); !ok {
		t.Fatal("queued-only finish of a queued run")
	}
	if ok, _, err := db.FinishRunWithEvent(ctx, "r1", Fence{Token: r1.Token}, done, nil); !ok || err != nil {
		t.Fatalf("finish: %v %v", ok, err)
	}
}

func TestLogChunksAndFences(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	if _, _, _, err := db.SubmitRunWithEvent(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "nb.ipynb",
		CreatedAt: time.Now()}, nil, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := db.ClaimRun(ctx, "w1", time.Minute)
	// Not running yet: nothing a worker writes is accepted.
	if err := db.AppendLogChunk(ctx, "r1", r.Token, LogChunk{FirstSeq: 1, LastSeq: 1, Text: "a\n"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("chunk before start: %v", err)
	}
	_, _, _ = db.MarkRunningWithEvent(ctx, "r1", r.Token, time.Now(), nil)
	for _, c := range []LogChunk{{1, 2, "a\nb\n"}, {3, 3, "c\n"}, {3, 3, "dup\n"}} {
		if err := db.AppendLogChunk(ctx, "r1", r.Token, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AppendLogChunk(ctx, "r1", r.Token+1, LogChunk{FirstSeq: 4, LastSeq: 4, Text: "x\n"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("chunk with a stale token: %v", err)
	}
	chunks, err := db.LogChunks(ctx, "r1", 2)
	if err != nil || len(chunks) != 1 || chunks[0].Text != "c\n" || !slices.Equal(chunks[0].Lines(), []string{"c"}) {
		t.Fatalf("chunks after 2: %+v %v", chunks, err)
	}
	if all, _ := db.LogChunks(ctx, "r1", 0); len(all) != 2 || !slices.Equal(all[0].Lines(), []string{"a", "b"}) {
		t.Fatalf("all chunks %+v", all)
	}

	// Fenced file writes.
	if err := db.PutRunFiles(ctx, "r1", r.Token+1, []ContentRef{{Path: "log.txt", SHA256: sha('a'), Size: 1}}); !errors.Is(err, ErrFenced) {
		t.Fatalf("outputs with a stale token: %v", err)
	}
	if err := db.PutRunFiles(ctx, "r1", r.Token, []ContentRef{{Path: "log.txt", SHA256: sha('a'), Size: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyRunChanges(ctx, "s1", []FileWrite{{Path: "x", SHA256: sha('b'), Size: 1}}, nil, nil, "r1", time.Now(), 0,
		Lease{RunID: "r1", Token: r.Token + 1}); !errors.Is(err, ErrFenced) {
		t.Fatalf("write-back with a stale token: %v", err)
	}
	if err := db.DeleteLogChunks(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if all, _ := db.LogChunks(ctx, "r1", 0); len(all) != 0 {
		t.Fatal("chunks left")
	}
}

func TestNotifyAndExclusive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := openTest(t)
	got := make(chan [2]string, 16)
	go db.Listen(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), func(c, p string) { got <- [2]string{c, p} }, ChanCancel, ChanQueued)
	next := func() [2]string {
		t.Helper()
		select {
		case n := <-got:
			return n
		case <-time.After(5 * time.Second):
			t.Fatal("no notification")
			return [2]string{}
		}
	}
	if n := next(); n != [2]string{"", ""} {
		t.Fatalf("first call %v, want the (re)connected signal", n)
	}
	if err := db.Notify(ctx, ChanCancel, "r9"); err != nil {
		t.Fatal(err)
	}
	if n := next(); n != [2]string{ChanCancel, "r9"} {
		t.Fatalf("notification %v", n)
	}

	// One holder at a time.
	inner := false
	ran, err := db.TryExclusive(ctx, 42, func(ctx context.Context) {
		inner, _ = db.TryExclusive(ctx, 42, func(context.Context) {})
	})
	if !ran || err != nil || inner {
		t.Fatalf("exclusive: ran %v inner %v %v", ran, inner, err)
	}
	if ran, _ := db.TryExclusive(ctx, 42, func(context.Context) {}); !ran {
		t.Fatal("the lock wasn't released")
	}
}

func TestWorkers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	for _, w := range []Worker{{ID: "w1", Up: false, Runtime: "{}", Manifest: "m1"}, {ID: "w2", Up: true, Runtime: `{"a":1}`, Manifest: "m2"}} {
		if err := db.PutWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := db.LiveWorkers(ctx, time.Minute)
	if err != nil || len(ws) != 2 || ws[0].ID != "w2" || ws[0].Manifest != "m2" || ws[0].SeenAt.IsZero() {
		t.Fatalf("live %+v %v", ws, err)
	}
	if err := db.DeleteWorker(ctx, "w2", time.Hour); err != nil {
		t.Fatal(err)
	}
	if ws, _ := db.LiveWorkers(ctx, time.Minute); len(ws) != 1 || ws[0].ID != "w1" {
		t.Fatalf("after delete %+v", ws)
	}
	if ws, _ := db.LiveWorkers(ctx, -time.Second); len(ws) != 0 {
		t.Fatalf("none seen in the future %+v", ws)
	}
}
