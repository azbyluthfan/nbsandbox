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

func TestSpaceOwnership(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1") // owned by alice

	if _, err := db.GetSpace(ctx, "alice", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSpace(ctx, "bob", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob reading alice's space: %v, want ErrNotFound", err)
	}
	if err := db.RenameSpace(ctx, "bob", "s1", "mine now", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob renaming alice's space: %v", err)
	}
	if err := db.DeleteSpace(ctx, "bob", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob deleting alice's space: %v", err)
	}
	if err := db.RenameSpace(ctx, "alice", "s1", "forecasts", time.Now()); err != nil {
		t.Fatal(err)
	}
	sp, _ := db.GetSpace(ctx, "alice", "s1")
	if sp.Name != "forecasts" {
		t.Fatalf("name = %q", sp.Name)
	}
}

func TestDeleteSpaceRefusesActiveRun(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	_ = db.InsertRun(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", CreatedAt: time.Now()})

	if err := db.DeleteSpace(ctx, "alice", "s1"); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("delete with queued run: %v, want ErrActiveRun", err)
	}
	_, _ = db.FinishRun(ctx, "r1", Outcome{Status: "cancelled", FinishedAt: time.Now()})
	if n, _ := db.CountRuns(ctx, "s1"); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if err := db.DeleteSpace(ctx, "alice", "s1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountRuns(ctx, "s1"); n != 0 {
		t.Fatalf("run rows must be deleted with the space, got %d", n)
	}
	if _, err := db.GetSpace(ctx, "alice", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("space still there")
	}
}

// Stable pages, newest first, no overlap.
func TestListSpacesPaging(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.UnixMilli(1_800_000_000_000)
	for i := 0; i < 7; i++ {
		// Two spaces share each timestamp, so the id tie-break is exercised.
		at := base.Add(time.Duration(i/2) * time.Second)
		if err := db.CreateSpace(ctx, Space{ID: fmt.Sprintf("s%d", i), Owner: "alice", CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.CreateSpace(ctx, Space{ID: "bobs", Owner: "bob", CreatedAt: base, UpdatedAt: base})

	var got []string
	cursor := ""
	pages := 0
	for {
		page, err := db.ListSpaces(ctx, "alice", cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, sp := range page.Items {
			got = append(got, sp.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := "s6 s5 s4 s3 s2 s1 s0"
	if fmt.Sprint(got) != "["+want+"]" || pages != 3 {
		t.Fatalf("got %v in %d pages, want [%s] in 3", got, pages, want)
	}
	for _, bad := range []string{"!!!", "bm9jb2xvbg", "YWJjOg"} {
		if _, err := db.ListSpaces(ctx, "alice", bad, 3); !errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %q: %v, want ErrBadCursor", bad, err)
		}
	}
	ids, _ := db.SpaceIDs(ctx)
	if len(ids) != 8 {
		t.Fatalf("SpaceIDs = %d", len(ids))
	}
}

// Many creates for one owner at the same moment never go past the cap.
func TestCreateSpaceCappedRace(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	owner := fmt.Sprintf("capped-%d", time.Now().UnixNano())
	errs := make(chan error, 12)
	for i := range 12 {
		go func() {
			now := time.Now()
			errs <- db.CreateSpaceCapped(ctx, Space{ID: fmt.Sprintf("%s-%d", owner, i), Owner: owner, CreatedAt: now, UpdatedAt: now}, 3)
		}()
	}
	ok, limited := 0, 0
	for range 12 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrSpaceLimit):
			limited++
		default:
			t.Fatal(err)
		}
	}
	if ok != 3 || limited != 9 {
		t.Fatalf("%d created, %d refused; want 3 and 9", ok, limited)
	}
	page, err := db.ListAllSpaces(ctx, owner, "", 50)
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("listed %d %v", len(page.Items), err)
	}
	// Without a cap, and the owner changes hands.
	now := time.Now()
	if err := db.CreateSpaceCapped(ctx, Space{ID: owner + "-free", Owner: owner, CreatedAt: now, UpdatedAt: now}, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSpaceOwner(ctx, owner+"-free", owner+"-new", now); err != nil {
		t.Fatal(err)
	}
	if o, err := db.SpaceOwner(ctx, owner+"-free"); err != nil || o != owner+"-new" {
		t.Fatalf("owner %q %v", o, err)
	}
	if err := db.SetSpaceOwner(ctx, "missing", "x", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing space: %v", err)
	}
	if _, err := db.SpaceOwner(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing space: %v", err)
	}
	all, err := db.ListAllSpaces(ctx, "", "", 200)
	if err != nil || len(all.Items) < 4 {
		t.Fatalf("all spaces: %d %v", len(all.Items), err)
	}
}
