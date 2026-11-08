// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSubmitRunIdempotency(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	now := time.Now()
	idem := &Idempotency{Owner: "alice", Key: "k1", BodyHash: "h1", Since: now.Add(-24 * time.Hour)}

	id, replay, err := db.SubmitRun(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now}, idem)
	if err != nil || replay || id != "r1" {
		t.Fatalf("first: %q %v %v", id, replay, err)
	}
	// Same key, same body: the original run, even though the space is busy.
	id, replay, err = db.SubmitRun(ctx, NewRun{ID: "r2", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now}, idem)
	if err != nil || !replay || id != "r1" {
		t.Fatalf("replay: %q %v %v", id, replay, err)
	}
	// Same key, different body.
	other := *idem
	other.BodyHash = "h2"
	if _, _, err := db.SubmitRun(ctx, NewRun{ID: "r3", SpaceID: "s1", Owner: "alice", Notebook: "b.ipynb", CreatedAt: now}, &other); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	// Keys are per user.
	bobs := *idem
	bobs.Owner = "bob"
	seedSpaceFor(t, db, "s2", "bob")
	if id, replay, err := db.SubmitRun(ctx, NewRun{ID: "r4", SpaceID: "s2", Owner: "bob", Notebook: "a.ipynb", CreatedAt: now}, &bobs); err != nil || replay || id != "r4" {
		t.Fatalf("bob's key: %q %v %v", id, replay, err)
	}
	// An expired key starts a new run.
	_, _ = db.FinishRun(ctx, "r1", Outcome{Status: "succeeded", FinishedAt: now})
	expired := *idem
	expired.Since = now.Add(time.Hour)
	if id, replay, err := db.SubmitRun(ctx, NewRun{ID: "r5", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now}, &expired); err != nil || replay || id != "r5" {
		t.Fatalf("expired key: %q %v %v", id, replay, err)
	}
	// Without a key, the busy space is a conflict and nothing is recorded.
	if _, _, err := db.SubmitRun(ctx, NewRun{ID: "r6", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now}, nil); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("busy: %v", err)
	}
	if n, _ := db.DeleteIdempotencyBefore(ctx, now.Add(time.Hour)); n != 2 {
		t.Fatalf("deleted %d keys, want 2", n)
	}
}

func seedSpaceFor(t *testing.T, db *DB, id, owner string) {
	t.Helper()
	now := time.Now()
	if err := db.CreateSpace(context.Background(), Space{ID: id, Owner: owner, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func TestRunQueries(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	base := time.UnixMilli(1_800_000_000_000)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("r%d", i)
		_, _, _ = db.SubmitRun(ctx, NewRun{ID: id, SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: base.Add(time.Duration(i) * time.Minute)}, nil)
		if i < 4 {
			_, _ = db.MarkRunning(ctx, id, base)
			_, _ = db.FinishRun(ctx, id, Outcome{Status: []string{"succeeded", "failed", "succeeded", "oom"}[i], FinishedAt: base.Add(time.Duration(i) * time.Hour)})
		}
	}
	r, err := db.GetRun(ctx, "s1", "r0")
	if err != nil || r.Status != "succeeded" || r.StartedAt == nil || r.FinishedAt == nil || !r.Terminal() {
		t.Fatalf("GetRun: %+v %v", r, err)
	}
	if _, err := db.GetRun(ctx, "other", "r0"); !errors.Is(err, ErrNotFound) {
		t.Fatal("run looked up through the wrong space")
	}
	page, _ := db.ListRuns(ctx, "s1", RunFilter{Status: "succeeded"}, "", 10)
	if len(page.Items) != 2 || page.Items[0].ID != "r2" {
		t.Fatalf("status filter: %+v", page.Items)
	}
	after := base.Add(90 * time.Second)
	before := base.Add(210 * time.Second)
	page, _ = db.ListRuns(ctx, "s1", RunFilter{CreatedAfter: &after, CreatedBefore: &before}, "", 10)
	if len(page.Items) != 2 || page.Items[0].ID != "r3" || page.Items[1].ID != "r2" {
		t.Fatalf("time filter: %+v", page.Items)
	}
	var all []string
	cursor := ""
	for {
		page, err := db.ListRuns(ctx, "s1", RunFilter{}, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Items {
			all = append(all, r.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if fmt.Sprint(all) != "[r4 r3 r2 r1 r0]" {
		t.Fatalf("paging: %v", all)
	}
	queued, _ := db.RunsWithStatus(ctx, "queued")
	if len(queued) != 1 || queued[0].ID != "r4" {
		t.Fatalf("queued: %+v", queued)
	}
	old, _ := db.FinishedBefore(ctx, base.Add(150*time.Minute))
	if len(old) != 3 {
		t.Fatalf("finished before: %d", len(old))
	}
	if err := db.DeleteRun(ctx, "s1", "r4"); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("delete queued run: %v", err)
	}
	if err := db.DeleteRun(ctx, "s1", "r0"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRun(ctx, "s1", "r0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if r, _ := db.RunByID(ctx, "r1"); r.SpaceID != "s1" {
		t.Fatal("RunByID")
	}
}
