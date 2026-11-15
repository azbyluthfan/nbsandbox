// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A set is asked for (pending), one worker builds it, a lapsed lease is
// taken over, and an old failure is asked for again.
func TestPackageSetBuilds(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	now := time.UnixMilli(time.Now().UnixMilli())
	if _, err := db.GetPackageSet(ctx, "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	p, err := db.RequestPackageSet(ctx, PackageRequest{Key: "k1", Requirements: "requests\n", PreferLock: "requests==2.32.3\n"}, 10*time.Minute, now)
	if err != nil || p.Status != PackagesPending || p.PreferLock == "" {
		t.Fatalf("request: %+v %v", p, err)
	}
	if again, _ := db.RequestPackageSet(ctx, PackageRequest{Key: "k1", Requirements: "requests\n"}, 10*time.Minute, now); again.Status != PackagesPending {
		t.Fatalf("asked twice: %+v", again)
	}
	p, err = db.ClaimPackageBuild(ctx, "w1", time.Minute, now)
	if err != nil || p.Key != "k1" || p.Status != PackagesBuilding || p.Builder != "w1" {
		t.Fatalf("claim: %+v %v", p, err)
	}
	if _, err := db.ClaimPackageBuild(ctx, "w2", time.Minute, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a live build was claimed twice: %v", err)
	}
	// w1's lease runs out: w2 takes over, and w1 can no longer finish.
	later := now.Add(2 * time.Minute)
	if p, err := db.ClaimPackageBuild(ctx, "w2", time.Minute, later); err != nil || p.Builder != "w2" {
		t.Fatalf("take over: %+v %v", p, err)
	}
	if ok, _ := db.FinishPackageBuild(ctx, "k1", "w1", true, "x", 1, "", "", later); ok {
		t.Fatal("w1 finished a build it lost")
	}
	if ok, _ := db.RenewPackageBuild(ctx, "k1", "w2", later.Add(time.Minute)); !ok {
		t.Fatal("renew")
	}
	if ok, err := db.FinishPackageBuild(ctx, "k1", "w2", true, "requests==2.32.3 --hash=sha256:aa\n", 12345, "Successfully installed", "", later); !ok || err != nil {
		t.Fatalf("finish: %v %v", ok, err)
	}
	p, _ = db.GetPackageSet(ctx, "k1")
	if p.Status != PackagesReady || p.SizeBytes != 12345 || p.Lock == "" || !p.BuiltAt.Equal(later) {
		t.Fatalf("ready: %+v", p)
	}
	if again, _ := db.RequestPackageSet(ctx, PackageRequest{Key: "k1", Requirements: "requests\n"}, 10*time.Minute, later.Add(time.Hour)); again.Status != PackagesReady {
		t.Fatal("a ready set is never rebuilt")
	}

	// A failure is kept for a while (callers see it), then may be asked for again.
	_, _ = db.RequestPackageSet(ctx, PackageRequest{Key: "k2", Requirements: "nope\n"}, 10*time.Minute, now)
	_, _ = db.ClaimPackageBuild(ctx, "w1", time.Minute, now)
	_, _ = db.FinishPackageBuild(ctx, "k2", "w1", false, "", 0, "ERROR: No matching distribution", "no wheel for nope", now)
	if p, _ := db.RequestPackageSet(ctx, PackageRequest{Key: "k2", Requirements: "nope\n"}, 10*time.Minute, now.Add(time.Minute)); p.Status != PackagesFailed || p.Error != "no wheel for nope" {
		t.Fatalf("recent failure: %+v", p)
	}
	if p, _ := db.RequestPackageSet(ctx, PackageRequest{Key: "k2", Requirements: "nope\n"}, 10*time.Minute, now.Add(11*time.Minute)); p.Status != PackagesPending || p.Error != "" {
		t.Fatalf("asked again after the wait: %+v", p)
	}

	// Retention: unused finished sets.
	_ = db.TouchPackageSet(ctx, "k1", later.Add(48*time.Hour))
	if keys, err := db.UnusedPackageSets(ctx, later.Add(24*time.Hour)); err != nil || len(keys) != 0 {
		t.Fatalf("k1 was used: %v %v", keys, err)
	}
	if keys, _ := db.UnusedPackageSets(ctx, later.Add(72*time.Hour)); len(keys) != 1 || keys[0] != "k1" {
		t.Fatalf("unused: %v", keys)
	}
	if err := db.DeletePackageSet(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetPackageSet(ctx, "k1"); !errors.Is(err, ErrNotFound) {
		t.Error("deleted")
	}
}

// A run that needs a set isn't claimed until the set is ready.
func TestRunsWaitForTheirPackages(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	seedSpace(t, db, "s1")
	now := time.UnixMilli(time.Now().UnixMilli())
	if _, err := db.RequestPackageSet(ctx, PackageRequest{Key: "k", Requirements: "requests\n"}, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.SubmitRunWithEvent(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "a.ipynb", ParametersJSON: "{}",
		CreatedAt: now, PackageKey: "k"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimRun(ctx, "w1", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claimed before its packages were ready: %v", err)
	}
	waiting, err := db.QueuedRunsWaitingFor(ctx, "k")
	if err != nil || len(waiting) != 1 || waiting[0].PackageKey != "k" {
		t.Fatalf("waiting: %+v %v", waiting, err)
	}
	_, _ = db.ClaimPackageBuild(ctx, "w1", time.Minute, now)
	_, _ = db.FinishPackageBuild(ctx, "k", "w1", true, "requests==2.32.3\n", 1, "", "", now)
	if r, err := db.ClaimRun(ctx, "w1", time.Minute); err != nil || r.ID != "r1" {
		t.Fatalf("claim once ready: %+v %v", r, err)
	}

	// A space's last requirements and lock.
	if err := db.SetSpacePackages(ctx, "s1", "requests\n", "requests==2.32.3\n"); err != nil {
		t.Fatal(err)
	}
	if req, lock, err := db.SpacePackages(ctx, "s1"); err != nil || req != "requests\n" || lock != "requests==2.32.3\n" {
		t.Errorf("space packages: %q %q %v", req, lock, err)
	}
}
