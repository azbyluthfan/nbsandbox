// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Shared spaces: what each role may do, and the rules for members.
func TestRoles(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, err := e.svc.Create(ctx, "alice", "shared")
	if err != nil {
		t.Fatal(err)
	}
	write := func(who, p string) error {
		_, err := e.svc.WriteFile(ctx, who, sp.ID, WriteRequest{Path: p, Body: strings.NewReader("x"), IfNoneMatch: true})
		return err
	}
	forbidden := func(err error) bool {
		var f *ForbiddenError
		return errors.As(err, &f)
	}
	if err := write("alice", "a.txt"); err != nil {
		t.Fatal(err)
	}

	// No role: the space doesn't exist for them.
	for _, err := range []error{
		func() error { _, err := e.svc.Get(ctx, "bob", sp.ID); return err }(),
		func() error { _, err := e.svc.ListFiles(ctx, "bob", sp.ID); return err }(),
		write("bob", "b.txt"),
		func() error { _, err := e.svc.Members(ctx, "bob", sp.ID); return err }(),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("no role: %v, want ErrNotFound", err)
		}
	}

	// read: sees, can't change.
	if _, err := e.svc.PutMember(ctx, "alice", sp.ID, "bob", RoleRead); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.Get(ctx, "bob", sp.ID)
	if err != nil || got.Role != RoleRead || got.Owner != "alice" {
		t.Fatalf("bob's view: %+v %v", got, err)
	}
	if files, err := e.svc.ListFiles(ctx, "bob", sp.ID); err != nil || len(files) != 1 {
		t.Fatalf("read lists files: %v %v", files, err)
	}
	if err := write("bob", "b.txt"); !forbidden(err) {
		t.Fatalf("read writes: %v", err)
	}
	if _, err := e.svc.Rename(ctx, "bob", sp.ID, "mine"); !forbidden(err) {
		t.Fatalf("read renames: %v", err)
	}
	if _, err := e.svc.PutMember(ctx, "bob", sp.ID, "carol", RoleRead); !forbidden(err) {
		t.Fatalf("read grants: %v", err)
	}

	// write: changes files, not members or the space.
	if _, err := e.svc.PutMember(ctx, "alice", sp.ID, "bob", RoleWrite); err != nil {
		t.Fatal(err)
	}
	if err := write("bob", "b.txt"); err != nil {
		t.Fatalf("write writes: %v", err)
	}
	if err := e.svc.Delete(ctx, "bob", sp.ID); !forbidden(err) {
		t.Fatalf("write deletes the space: %v", err)
	}

	// owner: members and the space; the creator stays.
	if _, err := e.svc.PutMember(ctx, "alice", sp.ID, "bob", RoleOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMember(ctx, "bob", sp.ID, "carol", RoleRead); err != nil {
		t.Fatalf("an owner grants: %v", err)
	}
	if _, err := e.svc.PutMember(ctx, "bob", sp.ID, "alice", RoleRead); err == nil {
		t.Fatal("the creator's role changed")
	}
	if err := e.svc.RemoveMember(ctx, "bob", sp.ID, "alice"); err == nil {
		t.Fatal("the creator was removed")
	}
	ms, err := e.svc.Members(ctx, "carol", sp.ID)
	if err != nil || len(ms) != 3 || ms[0].Principal != "alice" || !ms[0].Creator || ms[1].Principal != "bob" || ms[1].GrantedBy != "alice" {
		t.Fatalf("members: %+v %v", ms, err)
	}

	// Listing: each with the caller's role; filtered by role.
	list, _, err := e.svc.List(ctx, "carol", "", "", 10)
	if err != nil || len(list) != 1 || list[0].Role != RoleRead {
		t.Fatalf("carol's spaces: %+v %v", list, err)
	}
	if list, _, _ := e.svc.List(ctx, "carol", RoleOwner, "", 10); len(list) != 0 {
		t.Fatalf("carol owns: %+v", list)
	}
	if _, _, err := e.svc.List(ctx, "carol", "admin", "", 10); err == nil {
		t.Fatal("an unknown role filter")
	}

	// Leaving; revoking someone else needs owner.
	if err := e.svc.RemoveMember(ctx, "carol", sp.ID, "bob"); !forbidden(err) {
		t.Fatalf("read revokes another: %v", err)
	}
	if err := e.svc.RemoveMember(ctx, "carol", sp.ID, "carol"); err != nil {
		t.Fatalf("leaving: %v", err)
	}
	if _, err := e.svc.Get(ctx, "carol", sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after leaving: %v", err)
	}

	// Handing over: only the creator, only to an owner; the creator stays an owner.
	if _, err := e.svc.Transfer(ctx, "bob", sp.ID, "bob"); !forbidden(err) {
		t.Fatalf("a non-creator hands over: %v", err)
	}
	if _, err := e.svc.Transfer(ctx, "alice", sp.ID, "dave"); err == nil {
		t.Fatal("handed over to a non-owner")
	}
	if _, err := e.svc.Transfer(ctx, "alice", sp.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.Get(ctx, "alice", sp.ID); err != nil || got.Owner != "bob" || got.Role != RoleOwner {
		t.Fatalf("after handing over: %+v %v", got, err)
	}
	if err := e.svc.RemoveMember(ctx, "bob", sp.ID, "alice"); err != nil {
		t.Fatalf("the former creator can be removed now: %v", err)
	}

	// Bad input.
	for _, p := range []string{"", " bob", strings.Repeat("x", MaxPrincipal+1), "bo\nb"} {
		if _, err := e.svc.PutMember(ctx, "bob", sp.ID, p, RoleRead); err == nil {
			t.Errorf("principal %q accepted", p)
		}
	}
	if _, err := e.svc.PutMember(ctx, "bob", sp.ID, "erin", "admin"); err == nil {
		t.Error("role admin accepted")
	}
}
