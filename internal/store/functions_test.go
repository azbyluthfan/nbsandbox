// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFunctionVersions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	now := time.Now()
	if v, err := db.NextFunctionVersion(ctx, "s1", "f"); err != nil || v != 1 {
		t.Fatalf("next for a new function: %d %v", v, err)
	}
	fv := FunctionVersion{SpaceID: "s1", Function: "f", Version: 1, Notebook: "nb.ipynb", FilesJSON: `["m.pkl"]`,
		ContractInput: `{"type":"object"}`, NotebookSHA256: "abc", BundleBytes: 10, CreatedBy: "alice", CreatedAt: now}
	if err := db.AddFunctionVersion(ctx, fv); err != nil {
		t.Fatal(err)
	}
	// The same number again (a racing publish) is refused.
	if err := db.AddFunctionVersion(ctx, fv); !errors.Is(err, ErrVersionTaken) {
		t.Fatalf("version taken twice: %v", err)
	}
	fv.Version, fv.NotebookSHA256 = 2, "def"
	if err := db.AddFunctionVersion(ctx, fv); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.NextFunctionVersion(ctx, "s1", "f"); v != 3 {
		t.Fatalf("next = %d", v)
	}
	f, err := db.GetFunction(ctx, "s1", "f")
	if err != nil || f.LatestVersion != 2 {
		t.Fatalf("function %+v %v", f, err)
	}
	if latest, _ := db.GetFunctionVersion(ctx, "s1", "f", 0); latest.Version != 2 || latest.NotebookSHA256 != "def" {
		t.Fatalf("latest %+v", latest)
	}
	if v1, _ := db.GetFunctionVersion(ctx, "s1", "f", 1); v1.ContractInput != `{"type":"object"}` || v1.FilesJSON != `["m.pkl"]` {
		t.Fatalf("v1 %+v", v1)
	}
	if _, err := db.GetFunctionVersion(ctx, "s1", "f", 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
	if _, err := db.GetFunction(ctx, "s1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing function: %v", err)
	}
	if list, _ := db.FunctionVersions(ctx, "s1", "f"); len(list) != 2 || list[0].Version != 2 {
		t.Fatalf("versions %+v", list)
	}
	if fs, _ := db.Functions(ctx, "s1"); len(fs) != 1 || fs[0].Name != "f" {
		t.Fatalf("functions %+v", fs)
	}

	// Deleting refuses while an invocation is active.
	if _, _, err := db.SubmitRun(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "nb.ipynb", Function: "f",
		Version: 1, ReadOnlySpace: true, CreatedAt: now}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteFunction(ctx, "s1", "f"); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("delete while active: %v", err)
	}
	if _, err := db.FinishRun(ctx, "r1", Outcome{Status: "succeeded", FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteFunction(ctx, "s1", "f"); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.FunctionVersions(ctx, "s1", "f"); len(list) != 0 {
		t.Fatal("versions left after delete")
	}
	if err := db.DeleteFunction(ctx, "s1", "f"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if r, _ := db.RunByID(ctx, "r1"); r.Function != "f" || r.Version != 1 || r.WritesSpace {
		t.Fatalf("past invocation changed: %+v", r)
	}
}

// Runs that see the space read-only don't count toward one active run per
// space; runs that write still do.
func TestReadOnlyRunsRunAlongside(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	now := time.Now()
	submit := func(id string, readOnly bool) error {
		_, _, err := db.SubmitRun(ctx, NewRun{ID: id, SpaceID: "s1", Owner: "alice", Notebook: "n", ReadOnlySpace: readOnly, CreatedAt: now}, nil)
		return err
	}
	if err := submit("w1", false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r1", "r2"} {
		if err := submit(id, true); err != nil {
			t.Fatalf("read-only run %s refused: %v", id, err)
		}
	}
	if err := submit("w2", false); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("second writer: %v", err)
	}
}

func TestEventsAndTransitions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	now := time.Now()
	ev := func(status string) *Event {
		return &Event{SpaceID: "s1", RunID: "r1", Function: "f", Status: status, Data: `{"status":"` + status + `"}`, CreatedAt: now}
	}
	_, _, seq1, err := db.SubmitRunWithEvent(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "n", Function: "f", CreatedAt: now}, nil, ev("queued"))
	if err != nil || seq1 == 0 {
		t.Fatalf("submit with event: %d %v", seq1, err)
	}
	ok, seq2, err := db.MarkRunningWithEvent(ctx, "r1", 0, now, ev("running"))
	if err != nil || !ok || seq2 <= seq1 {
		t.Fatalf("running: %v %d %v", ok, seq2, err)
	}
	// A transition that doesn't happen records no event.
	if ok, seq, _ := db.MarkRunningWithEvent(ctx, "r1", 0, now, ev("running")); ok || seq != 0 {
		t.Fatal("second mark-running recorded something")
	}
	if ok, err := db.RequestCancel(ctx, "r1", "alice", "superseded"); err != nil || !ok {
		t.Fatalf("request cancel: %v %v", ok, err)
	}
	if ok, _ := db.RequestCancel(ctx, "r1", "bob", "again"); ok {
		t.Fatal("cancel recorded twice")
	}
	ok, seq3, err := db.FinishRunWithEvent(ctx, "r1", Fence{}, Outcome{Status: "cancelled", FinishedAt: now, OutputBytes: 7}, ev("cancelled"))
	if err != nil || !ok || seq3 <= seq2 {
		t.Fatalf("finish: %v %d %v", ok, seq3, err)
	}
	if ok, seq, _ := db.FinishRunWithEvent(ctx, "r1", Fence{}, Outcome{Status: "succeeded", FinishedAt: now}, ev("succeeded")); ok || seq != 0 {
		t.Fatal("a losing finish recorded an event")
	}
	if _, _, err := db.FinishRunWithEvent(ctx, "r1", Fence{}, Outcome{Status: "running"}, nil); err == nil {
		t.Fatal("non-terminal finish accepted")
	}
	r, _ := db.RunByID(ctx, "r1")
	if r.CancelledBy != "alice" || r.CancelReason != "superseded" || r.OutputBytes != 7 {
		t.Fatalf("run %+v", r)
	}

	all, err := db.Events(ctx, EventFilter{SpaceID: "s1"})
	if err != nil || len(all) != 3 || all[0].Status != "queued" || all[2].Status != "cancelled" {
		t.Fatalf("events %+v %v", all, err)
	}
	if after, _ := db.Events(ctx, EventFilter{SpaceID: "s1", RunID: "r1", Function: "f", After: seq1, Limit: 1}); len(after) != 1 || after[0].Seq != seq2 {
		t.Fatalf("after %+v", after)
	}
	if other, _ := db.Events(ctx, EventFilter{SpaceID: "s1", Function: "g"}); len(other) != 0 {
		t.Fatal("function filter ignored")
	}
	if _, err := db.AddEvent(ctx, Event{SpaceID: "s1", RunID: "r1", Status: "x", Data: "{}", CreatedAt: now.Add(48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if n, err := db.DeleteEventsBefore(ctx, now.Add(time.Hour)); err != nil || n != 3 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if oldest, _ := db.OldestEventSeq(ctx); oldest <= seq3 {
		t.Fatalf("oldest after prune = %d", oldest)
	}
	if rs, _ := db.ListRuns(ctx, "s1", RunFilter{Function: "f"}, "", 10); len(rs.Items) != 1 {
		t.Fatalf("function filter on runs: %+v", rs.Items)
	}
}
