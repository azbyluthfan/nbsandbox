// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newSession(id, space, notebook, mode string) Session {
	return Session{ID: id, SpaceID: space, Owner: "alice", Notebook: notebook, Mode: mode, CreatedAt: time.Now()}
}

func TestOpenSessionOnePerNotebook(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	s, created, err := db.OpenSession(ctx, newSession("x1", "s1", "a.ipynb", SessionReadWrite), 0)
	if err != nil || !created || s.Mode != SessionReadWrite || s.Status != SessionStarting {
		t.Fatalf("%+v %v %v", s, created, err)
	}
	again, created, err := db.OpenSession(ctx, newSession("x2", "s1", "a.ipynb", SessionReadOnly), 0)
	if err != nil || created || again.ID != "x1" {
		t.Fatalf("the notebook's open session: %+v %v %v", again, created, err)
	}
	if _, _, err := db.OpenSession(ctx, newSession("x3", "nope", "a.ipynb", SessionReadOnly), 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing space: %v", err)
	}
}

func TestOpenSessionShareTheWriteLock(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	if _, _, err := db.OpenSession(ctx, newSession("x1", "s1", "a.ipynb", SessionReadWrite), 0); err != nil {
		t.Fatal(err)
	}
	// A second notebook gets a read-only session.
	s, _, err := db.OpenSession(ctx, newSession("x2", "s1", "b.ipynb", SessionReadWrite), 0)
	if err != nil || s.Mode != SessionReadOnly {
		t.Fatalf("%+v %v", s, err)
	}
	// No writing run while it's open; a read-only one is fine.
	_, _, err = db.SubmitRun(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: time.Now()}, nil)
	if !errors.Is(err, ErrWritingSession) || !errors.Is(err, ErrActiveRun) {
		t.Fatalf("writing run: %v", err)
	}
	if _, _, err := db.SubmitRun(ctx, NewRun{ID: "r2", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", ReadOnlySpace: true,
		CreatedAt: time.Now()}, nil); err != nil {
		t.Fatalf("read-only run: %v", err)
	}
	if ok, err := db.EndSession(ctx, "x1", "ended", "alice", time.Now()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, _, err := db.SubmitRun(ctx, NewRun{ID: "r3", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: time.Now()}, nil); err != nil {
		t.Fatalf("after the session ended: %v", err)
	}
	// And with a writing run queued, a new session is read-only.
	s, _, err = db.OpenSession(ctx, newSession("x4", "s1", "c.ipynb", SessionReadWrite), 0)
	if err != nil || s.Mode != SessionReadOnly {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestOpenSessionPerUserLimit(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	for i, nb := range []string{"a.ipynb", "b.ipynb"} {
		if _, _, err := db.OpenSession(ctx, newSession("x"+nb, "s1", nb, SessionReadOnly), 2); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, _, err := db.OpenSession(ctx, newSession("x3", "s1", "c.ipynb", SessionReadOnly), 2); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("third: %v", err)
	}
	// Returning an open session isn't a new one.
	if _, created, err := db.OpenSession(ctx, newSession("x4", "s1", "a.ipynb", SessionReadOnly), 2); err != nil || created {
		t.Fatal(created, err)
	}
}

func TestClaimSessionPerWorker(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	for _, nb := range []string{"a", "b", "c"} {
		s := newSession("x"+nb, "s1", nb+".ipynb", SessionReadOnly)
		s.CreatedAt = time.Now().Add(time.Duration(len(nb)) * time.Millisecond)
		if _, _, err := db.OpenSession(ctx, s, 0); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for range 2 {
		s, ok, err := db.ClaimSession(ctx, "w1", 2)
		if err != nil || !ok || s.Worker != "w1" {
			t.Fatalf("%+v %v %v", s, ok, err)
		}
		got[s.ID] = true
	}
	if _, ok, err := db.ClaimSession(ctx, "w1", 2); ok || err != nil {
		t.Fatalf("w1 is full: %v %v", ok, err)
	}
	s, ok, err := db.ClaimSession(ctx, "w2", 2)
	if !ok || err != nil || got[s.ID] {
		t.Fatalf("w2: %+v %v %v", s, ok, err)
	}
	if _, ok, _ := db.ClaimSession(ctx, "w2", 2); ok {
		t.Fatal("nothing left to claim")
	}
	open, err := db.OpenSessions(ctx, SessionFilter{Worker: "w1"})
	if err != nil || len(open) != 2 {
		t.Fatalf("%v %v", open, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	if _, _, err := db.OpenSession(ctx, newSession("x1", "s1", "a.ipynb", SessionReadOnly), 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.MarkSessionReady(ctx, "x1"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	at := time.Now().Add(-time.Hour)
	if ok, err := db.EndSession(ctx, "x1", "idle", "", at); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := db.EndSession(ctx, "x1", "again", "", time.Now()); ok {
		t.Fatal("ended twice")
	}
	if ok, _ := db.MarkSessionReady(ctx, "x1"); ok {
		t.Fatal("ready after end")
	}
	s, err := db.GetSession(ctx, "x1")
	if err != nil || s.Open() || s.EndReason != "idle" || s.EndedAt.UnixMilli() != at.UnixMilli() {
		t.Fatalf("%+v %v", s, err)
	}
	// The notebook can have a new session now.
	if _, created, err := db.OpenSession(ctx, newSession("x2", "s1", "a.ipynb", SessionReadOnly), 0); err != nil || !created {
		t.Fatal(created, err)
	}
	if n, err := db.DeleteEndedSessionsBefore(ctx, time.Now().Add(-time.Minute)); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := db.GetSession(ctx, "x1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAdoptSession(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	if _, _, err := db.OpenSession(ctx, newSession("x1", "s1", "a.ipynb", SessionReadOnly), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.ClaimSession(ctx, "old", 0); !ok || err != nil {
		t.Fatal(ok, err)
	}
	s, err := db.AdoptSession(ctx, "x1", "new")
	if err != nil || s.Worker != "new" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := db.EndSession(ctx, "x1", "done", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdoptSession(ctx, "x1", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("adopting an ended session: %v", err)
	}
}
