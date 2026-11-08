// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testURL is a fresh schema in the test database (as storetest.URL; that
// package can't be imported here without an import cycle).
func testURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("NBSANDBOX_TEST_DATABASE_URL")
	if base == "" {
		base = "postgres://nbsandbox:nbsandbox@127.0.0.1:55432/nbsandbox_test?sslmode=disable"
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.PingContext(context.Background()); err != nil {
		t.Fatalf("no test database (start one with `make test-db`): %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = admin.Close()
	})
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedSpace(t *testing.T, db *DB, id string) {
	t.Helper()
	now := time.Now()
	if err := db.CreateSpace(context.Background(), Space{ID: id, Owner: "alice", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	u := testURL(t)
	for i := 0; i < 2; i++ { // second open must not re-apply migrations
		db, err := Open(ctx, u)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		v, err := db.SchemaVersion(ctx)
		if err != nil || v != 7 {
			t.Fatalf("schema version = %d, %v; want 7", v, err)
		}
		if err := db.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}
}

func TestOneActiveRunPerSpace(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	seedSpace(t, db, "s2")
	now := time.Now()

	if err := db.InsertRun(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	err := db.InsertRun(ctx, NewRun{ID: "r2", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now})
	if !errors.Is(err, ErrActiveRun) {
		t.Fatalf("second active run in the same space: got %v, want ErrActiveRun", err)
	}
	// Another space is independent.
	if err := db.InsertRun(ctx, NewRun{ID: "r3", SpaceID: "s2", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Once r1 finishes, the space accepts a new run.
	if won, err := db.FinishRun(ctx, "r1", Outcome{Status: "succeeded", FinishedAt: now}); err != nil || !won {
		t.Fatalf("finish r1: %v %v", won, err)
	}
	if err := db.InsertRun(ctx, NewRun{ID: "r4", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now}); err != nil {
		t.Fatalf("new run after finish: %v", err)
	}
}

func TestStateTransitions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	now := time.Now()
	_ = db.InsertRun(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now})

	if ok, _ := db.MarkRunning(ctx, "r1", now); !ok {
		t.Fatal("queued -> running must succeed")
	}
	if ok, _ := db.MarkRunning(ctx, "r1", now); ok {
		t.Fatal("running -> running must not succeed")
	}
	if won, _ := db.FinishRun(ctx, "r1", Outcome{Status: "failed", FinishedAt: now}); !won {
		t.Fatal("running -> failed must succeed")
	}
	if won, _ := db.FinishRun(ctx, "r1", Outcome{Status: "cancelled", FinishedAt: now}); won {
		t.Fatal("a terminal status must never change")
	}
	if ok, _ := db.MarkRunning(ctx, "r1", now); ok {
		t.Fatal("terminal -> running must not succeed")
	}
	if s, _ := db.RunStatus(ctx, "r1"); s != "failed" {
		t.Fatalf("status = %q, want failed", s)
	}
	if _, err := db.FinishRun(ctx, "r1", Outcome{Status: "running"}); err == nil {
		t.Fatal("finishing with a non-terminal status must be rejected")
	}
	if _, err := db.RunStatus(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run: %v", err)
	}
}

// Cancel and completion racing: exactly one wins.
func TestFinishRunRaceHasExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	statuses := []string{"succeeded", "cancelled", "timeout", "interrupted"}

	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("r%d", i)
		if err := db.InsertRun(ctx, NewRun{ID: id, SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		_, _ = db.MarkRunning(ctx, id, time.Now())

		var winners atomic.Int32
		var winner atomic.Value
		var wg sync.WaitGroup
		for _, st := range statuses {
			wg.Add(1)
			go func() {
				defer wg.Done()
				won, err := db.FinishRun(ctx, id, Outcome{Status: st, FinishedAt: time.Now()})
				if err != nil {
					t.Error(err)
				}
				if won {
					winners.Add(1)
					winner.Store(st)
				}
			}()
		}
		wg.Wait()
		if winners.Load() != 1 {
			t.Fatalf("run %s: %d winners, want exactly 1", id, winners.Load())
		}
		if got, _ := db.RunStatus(ctx, id); got != winner.Load() {
			t.Fatalf("run %s: stored %q but winner was %q", id, got, winner.Load())
		}
	}
}

func TestStoreErrors(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	now := time.Now()

	if err := db.CreateSpace(ctx, Space{ID: "s1", Owner: "bob", CreatedAt: now, UpdatedAt: now}); err == nil {
		t.Fatal("duplicate space id must fail")
	}
	if err := db.InsertRun(ctx, NewRun{ID: "r1", SpaceID: "no-such-space", Owner: "alice", Notebook: "a.ipynb", CreatedAt: now}); err == nil ||
		errors.Is(err, ErrActiveRun) {
		t.Fatalf("run in a missing space must fail with a foreign-key error, got %v", err)
	}
	if ok, err := db.MarkRunning(ctx, "missing", now); ok || err != nil {
		t.Fatalf("MarkRunning(missing) = %v, %v", ok, err)
	}
	if won, err := db.FinishRun(ctx, "missing", Outcome{Status: "cancelled", FinishedAt: now}); won || err != nil {
		t.Fatalf("FinishRun(missing) = %v, %v", won, err)
	}
}

func TestOpenRefusesBadURLs(t *testing.T) {
	ctx := context.Background()
	for _, u := range []string{"", "/srv/nbsandbox.db", "mysql://x/y", "postgres://127.0.0.1:1/x?connect_timeout=1",
		"postgres://u@127.0.0.1:55432/x?search_path=Bad-Name"} {
		if _, err := Open(ctx, u); err == nil {
			t.Errorf("Open(%q) succeeded", u)
		}
	}
}

func TestClosedDB(t *testing.T) {
	db, err := Open(context.Background(), testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if db.Ping(context.Background()) == nil {
		t.Fatal("ping on a closed db must fail")
	}
}
