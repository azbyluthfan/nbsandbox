// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Shared spaces in the store: roles, listing with them, revoking, handing
// over (docs/plan-shared-spaces.md).
func TestSpaceMembers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpaceFor(t, db, "s1", "alice")
	seedSpaceFor(t, db, "s2", "bob")
	now := time.Now()

	if sp, err := db.SpaceAccess(ctx, "alice", "s1"); err != nil || sp.Role != RoleOwner || sp.Owner != "alice" {
		t.Fatalf("the creator: %+v %v", sp, err)
	}
	if _, err := db.SpaceAccess(ctx, "bob", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no role: %v", err)
	}
	if err := db.PutSpaceMember(ctx, "s1", Member{Principal: "bob", Role: RoleRead, GrantedBy: "alice", GrantedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSpaceMember(ctx, "s1", Member{Principal: "bob", Role: RoleWrite, GrantedBy: "alice", GrantedAt: now}); err != nil {
		t.Fatal(err) // a change, not a second row
	}
	if sp, err := db.SpaceAccess(ctx, "bob", "s1"); err != nil || sp.Role != RoleWrite {
		t.Fatalf("bob's role: %+v %v", sp, err)
	}
	ms, err := db.SpaceMembers(ctx, "s1")
	if err != nil || len(ms) != 1 || ms[0].Principal != "bob" || ms[0].Role != RoleWrite || ms[0].GrantedBy != "alice" {
		t.Fatalf("members: %+v %v", ms, err)
	}

	// bob lists his own space and alice's, each with his role.
	page, err := db.ListSpaces(ctx, "bob", "", "", 10)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("bob's spaces: %+v %v", page, err)
	}
	roles := map[string]string{}
	for _, sp := range page.Items {
		roles[sp.ID] = sp.Role
	}
	if roles["s1"] != RoleWrite || roles["s2"] != RoleOwner {
		t.Fatalf("roles: %v", roles)
	}
	if page, _ := db.ListSpaces(ctx, "bob", RoleWrite, "", 10); len(page.Items) != 1 || page.Items[0].ID != "s1" {
		t.Fatalf("bob's spaces with write: %+v", page.Items)
	}
	if page, _ := db.ListSpaces(ctx, "bob", "", "", 1); len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("paging: %+v", page)
	} else if next, err := db.ListSpaces(ctx, "bob", "", page.NextCursor, 1); err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatalf("the next page: %+v %v", next, err)
	}

	// Handing over: only from the creator; the creator stays an owner; the
	// new creator's grant goes.
	if err := db.TransferSpace(ctx, "s1", "bob", "bob", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("from someone not the creator: %v", err)
	}
	if err := db.TransferSpace(ctx, "s1", "alice", "bob", now); err != nil {
		t.Fatal(err)
	}
	if sp, _ := db.SpaceAccess(ctx, "bob", "s1"); sp.Owner != "bob" || sp.Role != RoleOwner {
		t.Fatalf("after handing over, bob: %+v", sp)
	}
	if sp, _ := db.SpaceAccess(ctx, "alice", "s1"); sp.Role != RoleOwner {
		t.Fatalf("after handing over, alice: %+v", sp)
	}
	if ms, _ := db.SpaceMembers(ctx, "s1"); len(ms) != 1 || ms[0].Principal != "alice" {
		t.Fatalf("members after handing over: %+v", ms)
	}

	// Revoking; an administrator's change of owner drops the new owner's grant.
	if err := db.DeleteSpaceMember(ctx, "s1", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSpaceMember(ctx, "s1", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice: %v", err)
	}
	if _, err := db.SpaceAccess(ctx, "alice", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after revoking: %v", err)
	}
	if err := db.PutSpaceMember(ctx, "s1", Member{Principal: "carol", Role: RoleRead, GrantedBy: "bob", GrantedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSpaceOwner(ctx, "s1", "carol", now); err != nil {
		t.Fatal(err)
	}
	if ms, _ := db.SpaceMembers(ctx, "s1"); len(ms) != 0 {
		t.Fatalf("the new owner kept a grant: %+v", ms)
	}

	// Which names each member bound, never to what.
	if err := db.SetSpaceNames(ctx, "s1", SpaceNames{Default: "dev", Environments: []string{"dev"}, Names: map[string]string{"crm": "postgresql"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertConnection(ctx, newConnection("c1", "carol", "crm")); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBindings(ctx, "s1", "carol", Bindings{"dev": {"crm": "c1"}}); err != nil {
		t.Fatal(err)
	}
	if b, err := db.BoundNames(ctx, "s1"); err != nil || len(b["carol"]["dev"]) != 1 || b["carol"]["dev"][0] != "crm" {
		t.Fatalf("bound names: %+v %v", b, err)
	}
	// Deleting the space takes its grants with it.
	if err := db.PutSpaceMember(ctx, "s1", Member{Principal: "dave", Role: RoleRead, GrantedBy: "carol", GrantedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSpace(ctx, "carol", "s1"); err != nil {
		t.Fatal(err)
	}
	if ms, err := db.SpaceMembers(ctx, "s1"); err != nil || len(ms) != 0 {
		t.Fatalf("grants outlived the space: %+v %v", ms, err)
	}
}
