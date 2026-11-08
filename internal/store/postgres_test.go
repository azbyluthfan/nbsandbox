// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestRebind(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":                              "SELECT 1",
		"a = ? AND b = ?":                       "a = $1 AND b = $2",
		"x = '?' AND y = ?":                     "x = '?' AND y = $1",
		"status IN ('queued', 'running') AND ?": "status IN ('queued', 'running') AND $1",
	} {
		if got := rebind(in); got != want {
			t.Errorf("rebind(%q) = %q, want %q", in, got, want)
		}
	}
}

// Events become visible in sequence order even with many concurrent
// writers: once a reader has seen event N, no event below N appears later
// (a stream resuming after N would otherwise miss it for good).
func TestEventsVisibleInOrder(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	const writers, each = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if _, err := db.AddEvent(ctx, Event{SpaceID: "s1", RunID: fmt.Sprintf("r%d-%d", w, i), Status: "queued", Data: "{}", CreatedAt: time.Now()}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var seen []int64
	var after int64
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		default:
		}
		evs, err := db.Events(ctx, EventFilter{SpaceID: "s1", After: after, Limit: 10000})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			seen = append(seen, ev.Seq)
			after = ev.Seq
		}
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// What a resuming reader collected must be everything, with no gaps.
	all, err := db.Events(ctx, EventFilter{SpaceID: "s1", Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != writers*each || len(seen) != len(all) {
		t.Fatalf("a reader resuming after the last event it saw got %d of %d events: some became visible after a later one",
			len(seen), len(all))
	}
	if !slices.IsSorted(seen) {
		t.Fatal("events seen out of order")
	}
}

// Concurrent writing runs in one space: exactly one is accepted.
func TestOneWriterUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	var ok, busy int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, err := db.SubmitRunWithEvent(ctx, NewRun{ID: fmt.Sprintf("r%d", i), SpaceID: "s1", Owner: "alice", Notebook: "nb.ipynb", CreatedAt: time.Now()}, nil, nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrActiveRun):
				busy++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || busy != 11 {
		t.Fatalf("%d accepted, %d refused as busy; want 1 and 11", ok, busy)
	}
}

// The same Idempotency-Key sent many times at once creates one run; every
// other request gets that run back.
func TestIdempotencyUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	idem := &Idempotency{Owner: "alice", Key: "k", BodyHash: "h", Since: time.Now().Add(-time.Hour)}
	ids := make([]string, 10)
	replays := make([]bool, 10)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, replay, _, err := db.SubmitRunWithEvent(ctx, NewRun{ID: fmt.Sprintf("r%d", i), SpaceID: "s1", Owner: "alice", Notebook: "nb.ipynb",
				CreatedAt: time.Now(), ReadOnlySpace: true}, idem, nil)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i], replays[i] = id, replay
		}()
	}
	wg.Wait()
	created := 0
	for i := range ids {
		if ids[i] != ids[0] {
			t.Fatalf("requests got different runs: %v", ids)
		}
		if !replays[i] {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d requests created a run; want exactly 1", created)
	}
}
