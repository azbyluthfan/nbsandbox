// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newConnection(id, owner, name string) Connection {
	now := time.Now()
	return Connection{ID: id, Owner: owner, Name: name, Type: "postgresql", Settings: `{"host":"db"}`,
		SecretMode: SecretManaged, SecretProvider: "builtin", SecretRef: "s-" + id, CreatedAt: now, UpdatedAt: now}
}

func TestConnectionsBelongToTheirOwner(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	if err := db.InsertConnection(ctx, newConnection("c1", "alice", "crm")); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertConnection(ctx, newConnection("c2", "alice", "crm")); !errors.Is(err, ErrConnectionName) {
		t.Fatalf("same name, same owner: %v", err)
	}
	if err := db.InsertConnection(ctx, newConnection("c3", "bob", "crm")); err != nil {
		t.Fatalf("same name, another owner: %v", err)
	}
	if _, err := db.GetConnection(ctx, "bob", "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob gets alice's: %v", err)
	}
	if list, err := db.ListConnections(ctx, "alice"); err != nil || len(list) != 1 || list[0].Name != "crm" || list[0].SecretRef != "s-c1" {
		t.Fatalf("alice's list: %+v %v", list, err)
	}
	c := newConnection("c1", "bob", "renamed")
	if err := db.UpdateConnection(ctx, c); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob updates alice's: %v", err)
	}
	c.Owner = "alice"
	if err := db.UpdateConnection(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetConnection(ctx, "alice", "c1"); got.Name != "renamed" {
		t.Fatalf("after the update: %+v", got)
	}
	if err := db.DeleteConnection(ctx, "bob", "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob deletes alice's: %v", err)
	}
}

func TestSpaceBindings(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpaceFor(t, db, "s1", "alice")
	for _, c := range []Connection{newConnection("dev", "alice", "crm-dev"), newConnection("prod", "alice", "crm-prod")} {
		if err := db.InsertConnection(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := db.GetSpaceNames(ctx, "s1")
	if err != nil || len(empty.Environments) != 0 || empty.Default != "" || len(empty.Names) != 0 {
		t.Fatalf("before: %+v %v", empty, err)
	}
	if err := db.SetSpaceNames(ctx, "s1", SpaceNames{Default: "dev", Environments: []string{"dev", "prod"}, Names: map[string]string{"crm": "postgresql"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBindings(ctx, "s1", "alice", Bindings{"dev": {"crm": "dev"}, "prod": {"crm": "prod"}}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetSpaceNames(ctx, "s1")
	if err != nil || got.Default != "dev" || len(got.Environments) != 2 || got.Names["crm"] != "postgresql" {
		t.Fatalf("after: %+v %v", got, err)
	}
	if b, err := db.GetBindings(ctx, "s1", "alice"); err != nil || b["prod"]["crm"] != "prod" {
		t.Fatalf("bindings: %+v %v", b, err)
	}
	if b, _ := db.GetBindings(ctx, "s1", "bob"); len(b) != 0 {
		t.Fatalf("bob's bindings: %+v", b)
	}
	if uses, err := db.ConnectionUses(ctx, "prod"); err != nil || len(uses) != 1 || uses[0].Environment != "prod" || uses[0].Name != "crm" {
		t.Fatalf("uses: %+v %v", uses, err)
	}
	// A bound connection can't be deleted.
	if err := db.DeleteConnection(ctx, "alice", "prod"); !errors.Is(err, ErrConnectionInUse) {
		t.Fatalf("delete while bound: %v", err)
	}
	// Dropping an environment drops its bindings: prod is free again; dev's stay.
	if err := db.SetSpaceNames(ctx, "s1", SpaceNames{Default: "dev", Environments: []string{"dev"}, Names: map[string]string{"crm": "postgresql"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteConnection(ctx, "alice", "prod"); err != nil {
		t.Fatalf("delete once unbound: %v", err)
	}
	if b, _ := db.GetBindings(ctx, "s1", "alice"); b["dev"]["crm"] != "dev" {
		t.Fatalf("dev's binding went: %+v", b)
	}
	// A binding to a connection that doesn't exist, or a name not declared, is refused, and nothing changes.
	for _, bad := range []Bindings{{"dev": {"crm": "gone"}}, {"dev": {"lake": "dev"}}} {
		if err := db.SetBindings(ctx, "s1", "alice", bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	if b, _ := db.GetBindings(ctx, "s1", "alice"); b["dev"]["crm"] != "dev" {
		t.Fatalf("a refused change changed something: %+v", b)
	}
	// A name whose type changes loses its bindings.
	if err := db.SetSpaceNames(ctx, "s1", SpaceNames{Default: "dev", Environments: []string{"dev"}, Names: map[string]string{"crm": "mysql"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := db.GetBindings(ctx, "s1", "alice"); len(b["dev"]) != 0 {
		t.Fatalf("a binding of the old type stayed: %+v", b)
	}
	if err := db.SetSpaceNames(ctx, "s1", SpaceNames{Default: "dev", Environments: []string{"dev"}, Names: map[string]string{"crm": "postgresql"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBindings(ctx, "s1", "alice", Bindings{"dev": {"crm": "dev"}}); err != nil {
		t.Fatal(err)
	}
	// Deleting the space removes its environments and bindings, freeing its connections.
	if err := db.DeleteSpace(ctx, "alice", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteConnection(ctx, "alice", "dev"); err != nil {
		t.Fatalf("delete after the space went: %v", err)
	}
}

func TestBuiltinSecrets(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	if _, _, err := db.GetBuiltinSecret(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := db.PutBuiltinSecret(ctx, "x", []byte("n1"), []byte("c1"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.PutBuiltinSecret(ctx, "x", []byte("n2"), []byte("c2"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, c, err := db.GetBuiltinSecret(ctx, "x"); err != nil || string(n) != "n2" || string(c) != "c2" {
		t.Fatalf("replaced: %q %q %v", n, c, err)
	}
	if err := db.DeleteBuiltinSecret(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.GetBuiltinSecret(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
