// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Shared spaces: owners declare names, each member binds their own
// connections, and a run resolves with its caller's
// (docs/plan-shared-spaces.md).
func TestNamesPerMember(t *testing.T) {
	ctx := context.Background()
	s, db, _ := newService(t)
	at := time.Now()
	if err := db.CreateSpace(ctx, store.Space{ID: "s1", Owner: "alice", Name: "team", CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	alices, _ := s.Create(ctx, "alice", pg("crm-alice", "pg.internal"))
	bobs, _ := s.Create(ctx, "bob", pg("crm-bob", "pg.internal"))
	bobsS3, err := s.Create(ctx, "bob", Input{Name: "lake", Type: TypeS3, Settings: map[string]any{"endpoint": "minio.internal:9000", "bucket": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	var forbidden *spaces.ForbiddenError

	// alice declares crm by binding it, as before shared spaces.
	if _, err := s.SetSpace(ctx, "alice", "s1", SpaceInput{Default: "dev", Environments: map[string]map[string]string{
		"dev": {"crm": alices.ID}, "prod": {"crm": alices.ID}}}); err != nil {
		t.Fatal(err)
	}
	// bob has no role yet: nothing.
	if _, err := s.Space(ctx, "bob", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob without a role: %v", err)
	}
	if err := db.PutSpaceMember(ctx, "s1", store.Member{Principal: "bob", Role: store.RoleRead, GrantedBy: "alice", GrantedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// bob sees the declarations, none of alice's connections, and what he must bind.
	v, err := s.Space(ctx, "bob", "s1")
	if err != nil || len(v.Environments["dev"]) != 0 || fmt.Sprint(v.Missing) != "map[dev:[crm] prod:[crm]]" {
		t.Fatalf("bob's view: %+v %v", v, err)
	}
	if _, _, err := s.Resolve(ctx, "bob", "s1", "", nil); err == nil || !strings.Contains(err.Error(), "bind your own connections to crm") {
		t.Fatalf("bob's run before binding: %v", err)
	}

	// He binds his own; not alice's, not another type, not undeclared names, not declaring.
	for name, bad := range map[string]SpaceInput{
		"alice's connection": {Environments: map[string]map[string]string{"dev": {"crm": alices.ID}}},
		"the wrong type":     {Environments: map[string]map[string]string{"dev": {"crm": bobsS3.ID}}},
		"an undeclared name": {Environments: map[string]map[string]string{"dev": {"lake": bobsS3.ID}}},
		"an undeclared env":  {Environments: map[string]map[string]string{"qa": {"crm": bobs.ID}}},
	} {
		if _, err := s.SetSpace(ctx, "bob", "s1", bad); !isInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.SetSpace(ctx, "bob", "s1", SpaceInput{Default: "dev", Environments: map[string]map[string]string{"dev": {"crm": bobs.ID}}}); !errors.As(err, &forbidden) {
		t.Fatalf("bob declares: %v", err)
	}
	if _, err := s.SetNames(ctx, "bob", "s1", NamesInput{Default: "dev", Environments: []string{"dev"}}); !errors.As(err, &forbidden) {
		t.Fatalf("bob sets the names: %v", err)
	}
	if v, err := s.SetSpace(ctx, "bob", "s1", SpaceInput{Environments: map[string]map[string]string{"dev": {"crm": "crm-bob"}}}); err != nil ||
		v.Environments["dev"]["crm"].ID != bobs.ID || fmt.Sprint(v.Missing) != "map[prod:[crm]]" {
		t.Fatalf("bob binds dev: %+v %v", v, err)
	}

	// Runs resolve with their caller's bindings.
	if env, names, err := s.Resolve(ctx, "bob", "s1", "", nil); err != nil || env != "dev" || names["crm"] != bobs.ID {
		t.Fatalf("bob's run: %q %v %v", env, names, err)
	}
	if _, names, err := s.Resolve(ctx, "alice", "s1", "", nil); err != nil || names["crm"] != alices.ID {
		t.Fatalf("alice's run: %v %v", names, err)
	}
	if _, _, err := s.Resolve(ctx, "bob", "s1", "prod", nil); err == nil {
		t.Fatal("bob's run in prod, unbound")
	}
	if _, _, err := s.Resolve(ctx, "bob", "s1", "prod", []string{}); err != nil {
		t.Fatalf("a function needing no names: %v", err)
	}
	// Others see which names bob bound, never to what.
	nv, err := s.Names(ctx, "alice", "s1")
	if err != nil || nv.Names["crm"] != "postgresql" || fmt.Sprint(nv.Bound["bob"]) != "map[dev:[crm]]" || fmt.Sprint(nv.Environments) != "[dev prod]" {
		t.Fatalf("names: %+v %v", nv, err)
	}

	// Owners change the declarations; bindings that no longer fit go.
	if _, err := s.SetNames(ctx, "alice", "s1", NamesInput{Default: "dev", Environments: []string{"dev"}, Names: map[string]string{"crm": "postgresql", "lake": "s3"}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Space(ctx, "bob", "s1"); v.Environments["dev"]["crm"].ID != bobs.ID || fmt.Sprint(v.Missing) != "map[dev:[lake]]" {
		t.Fatalf("after adding lake: %+v", v)
	}
	if _, err := s.SetSpace(ctx, "bob", "s1", SpaceInput{Environments: map[string]map[string]string{"dev": {"crm": bobs.ID, "lake": bobsS3.ID}}}); err != nil {
		t.Fatal(err)
	}
	// Leaving the space drops his bindings, so his connections can be deleted.
	if err := s.Delete(ctx, "bob", bobs.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete while bound: %v", err)
	}
	if err := db.DeleteSpaceMember(ctx, "s1", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "bob", bobs.ID); err != nil {
		t.Fatalf("delete after leaving: %v", err)
	}
	for _, bad := range []NamesInput{
		{Default: "qa", Environments: []string{"dev"}},
		{Environments: []string{"dev", "dev"}},
		{Environments: []string{"dev"}, Names: map[string]string{"crm": "oracle"}},
		{Names: map[string]string{"crm": "postgresql"}},
	} {
		if _, err := s.SetNames(ctx, "alice", "s1", bad); !isInvalid(err) {
			t.Errorf("names %+v: %v", bad, err)
		}
	}
}
