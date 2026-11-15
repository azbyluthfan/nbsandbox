// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAuditLog(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	base := time.UnixMilli(time.Now().UnixMilli())
	for i, e := range []AuditEvent{
		{Actor: "alice", Action: "CreateSpace", SpaceID: "s1", Status: 201},
		{Actor: "bob", Action: "GetFile", SpaceID: "s1", Target: "a.csv", Status: 404},
		{Actor: "alice", Action: "CreateRun", SpaceID: "s2", Target: "r1", Status: 202, Via: "grpc"},
	} {
		e.At = base.Add(time.Duration(i) * time.Minute)
		if err := db.InsertAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	all, next, err := db.ListAudit(ctx, AuditFilter{}, "", 10)
	if err != nil || len(all) != 3 || next != "" || all[0].Action != "CreateRun" || all[0].Via != "grpc" || !all[0].At.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("all: %+v %q %v", all, next, err)
	}
	page, next, _ := db.ListAudit(ctx, AuditFilter{}, "", 2)
	if len(page) != 2 || next == "" {
		t.Fatalf("page 1: %d %q", len(page), next)
	}
	rest, next2, _ := db.ListAudit(ctx, AuditFilter{}, next, 2)
	if len(rest) != 1 || rest[0].Action != "CreateSpace" || next2 != "" {
		t.Fatalf("page 2: %+v %q", rest, next2)
	}
	if got, _, _ := db.ListAudit(ctx, AuditFilter{Actor: "alice", SpaceID: "s1"}, "", 10); len(got) != 1 || got[0].Action != "CreateSpace" {
		t.Errorf("alice in s1: %+v", got)
	}
	since := base.Add(time.Minute)
	if got, _, _ := db.ListAudit(ctx, AuditFilter{Since: &since, Action: "GetFile"}, "", 10); len(got) != 1 || got[0].Status != 404 {
		t.Errorf("since, action: %+v", got)
	}
	if _, _, err := db.ListAudit(ctx, AuditFilter{}, "nope", 10); !errors.Is(err, ErrBadCursor) {
		t.Errorf("bad cursor: %v", err)
	}
	if n, err := db.DeleteAuditBefore(ctx, base.Add(90*time.Second)); err != nil || n != 2 {
		t.Errorf("retention: %d %v", n, err)
	}
}

// While drained, no worker claims a queued run; resuming lets them.
func TestDrainStopsClaims(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	seedSpace(t, db, "s1")
	if err := db.InsertRun(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", ParametersJSON: "{}", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(time.Now().UnixMilli())
	if err := db.SetDrain(ctx, true, "upgrade", "root", at); err != nil {
		t.Fatal(err)
	}
	_ = db.SetDrain(ctx, true, "upgrade, longer", "root", at.Add(time.Hour)) // keeps the start
	d, err := db.GetDrain(ctx)
	if err != nil || !d.Draining || d.Reason != "upgrade, longer" || d.By != "root" || !d.Since.Equal(at) || d.Queued != 1 || d.Running != 0 {
		t.Fatalf("drained: %+v %v", d, err)
	}
	if _, err := db.ClaimRun(ctx, "w1", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim while drained: %v", err)
	}
	if err := db.SetDrain(ctx, false, "", "root", at); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.GetDrain(ctx); d.Draining || !d.Since.IsZero() {
		t.Errorf("resumed: %+v", d)
	}
	if r, err := db.ClaimRun(ctx, "w1", time.Minute); err != nil || r.ID != "r1" {
		t.Fatalf("claim after resuming: %+v %v", r, err)
	}
}

func TestListRunsAcrossSpaces(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	seedSpace(t, db, "s1")
	seedSpace(t, db, "s2")
	now := time.Now()
	for i, r := range []NewRun{{ID: "r1", SpaceID: "s1", Owner: "alice"}, {ID: "r2", SpaceID: "s2", Owner: "bob"}} {
		r.Notebook, r.ParametersJSON, r.CreatedAt = "a.ipynb", "{}", now.Add(time.Duration(i)*time.Second)
		if err := db.InsertRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	all, err := db.ListRuns(ctx, "", RunFilter{}, "", 10)
	if err != nil || len(all.Items) != 2 || all.Items[0].ID != "r2" {
		t.Fatalf("all: %+v %v", all, err)
	}
	if bob, _ := db.ListRuns(ctx, "", RunFilter{Owner: "bob"}, "", 10); len(bob.Items) != 1 || bob.Items[0].ID != "r2" {
		t.Errorf("bob's: %+v", bob)
	}
}
