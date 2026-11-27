// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/secrets"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

func newService(t *testing.T) (*Service, *store.DB, *secrets.Builtin) {
	t.Helper()
	db := storetest.Open(t)
	b, err := secrets.NewBuiltin(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), db)
	if err != nil {
		t.Fatal(err)
	}
	return New(Deps{DB: db, Secrets: b, Allowed: NewAllowed([]string{"*.internal", "10.0.0.0/8", "127.0.0.0/8", "api.example.com"})}), db, b
}

func ptr(s string) *string { return &s }

func pg(name, host string) Input {
	return Input{Name: name, Type: TypePostgreSQL, Settings: map[string]any{"host": host, "database": "crm", "user": "reader"},
		Secret: SecretInput{Value: ptr("pa55")}}
}

func isInvalid(err error) bool {
	var ie *InvalidError
	return errors.As(err, &ie)
}

func TestAllowed(t *testing.T) {
	a := NewAllowed([]string{"*.corp.example", "db.internal", "10.0.0.0/8", "169.254.0.0/16", "127.0.0.0/8", "::/0"})
	for host, want := range map[string]bool{
		"db.internal": true, "other.internal": false, "x.corp.example": true, "corp.example": false,
		"10.1.2.3": true, "11.0.0.1": false,
		// never, whatever the list says
		"127.0.0.1": false, "169.254.169.254": false, "::1": false, "0.0.0.0": false, "localhost": false, "a.localhost": false, "fe80::1": false,
	} {
		if got := a.Permits(host); got != want {
			t.Errorf("Permits(%q) = %v", host, got)
		}
	}
}

func TestCreateAndSecrets(t *testing.T) {
	ctx := context.Background()
	s, db, b := newService(t)
	v, err := s.Create(ctx, "alice", pg("crm-prod", "pg.internal"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Settings["port"] != float64(5432) && v.Settings["port"] != 5432 || v.Secret.Mode != "managed" || v.Secret.Ref != "" {
		t.Fatalf("view: %+v", v)
	}
	out, _ := json.Marshal(v)
	if strings.Contains(string(out), "pa55") {
		t.Fatal("the view shows the secret")
	}
	rec, _ := db.GetConnection(ctx, "alice", v.ID)
	if strings.Contains(rec.Settings, "pa55") {
		t.Fatal("the record holds the secret")
	}
	if got, err := b.Reveal(ctx, rec.SecretRef); err != nil || string(got) != "pa55" {
		t.Fatalf("stored secret: %q %v", got, err)
	}

	for name, in := range map[string]Input{
		"no secret for a database":   {Name: "a", Type: TypePostgreSQL, Settings: map[string]any{"host": "pg.internal", "database": "d", "user": "u"}},
		"a reference (builtin)":      {Name: "b", Type: TypePostgreSQL, Settings: map[string]any{"host": "pg.internal", "database": "d", "user": "u"}, Secret: SecretInput{Ref: ptr("x#y")}},
		"value and reference":        {Name: "c", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com"}, Secret: SecretInput{Value: ptr("v"), Ref: ptr("x#y")}},
		"a host not allowed":         pg("d", "pg.elsewhere.com"),
		"loopback, even if allowed":  pg("e", "127.0.0.1"),
		"metadata address":           pg("f", "169.254.169.254"),
		"an unknown setting":         {Name: "g", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "password": "x"}},
		"a missing setting":          {Name: "h", Type: TypePostgreSQL, Settings: map[string]any{"host": "pg.internal"}, Secret: SecretInput{Value: ptr("v")}},
		"an unknown type":            {Name: "i", Type: "oracle", Settings: map[string]any{}},
		"a bad name":                 pg("Bad Name", "pg.internal"),
		"credentials in a URL":       {Name: "j", Type: TypeHTTP, Settings: map[string]any{"url": "https://u:p@api.example.com"}},
		"a scheme in an endpoint":    {Name: "k", Type: TypeS3, Settings: map[string]any{"endpoint": "https://minio.internal", "bucket": "b"}},
		"an unknown auth":            {Name: "l", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "auth": "digest"}},
		"basic without user":         {Name: "m", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "auth": "basic"}, Secret: SecretInput{Value: ptr("pw")}},
		"basic without password":     {Name: "n", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "auth": "basic", "user": "ann"}},
		"oauth2 without token_url":   {Name: "o", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "auth": "oauth2", "client_id": "c"}, Secret: SecretInput{Value: ptr("cs")}},
		"oauth2 token_url elsewhere": {Name: "p", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "auth": "oauth2", "client_id": "c", "token_url": "https://idp.elsewhere.com/token"}, Secret: SecretInput{Value: ptr("cs")}},
		"oauth2 without a secret":    {Name: "q", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "auth": "oauth2", "client_id": "c", "token_url": "https://idp.example.com/token"}},
		"none with a secret":         {Name: "r", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "auth": "none"}, Secret: SecretInput{Value: ptr("x")}},
		"user with header auth":      {Name: "s", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com", "user": "ann"}},
		"ca for plain http":          {Name: "t", Type: TypeHTTP, Settings: map[string]any{"url": "http://api.example.com", "ca": testCAPEM(t)}},
		"a fragment in the URL":      {Name: "u", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com/v1#x"}},
	} {
		if _, err := s.Create(ctx, "alice", in); !isInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.Create(ctx, "alice", pg("crm-prod", "pg.internal")); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("taken name: %v", err)
	}
	// An open API needs no secret; object storage defaults are filled in.
	if _, err := s.Create(ctx, "alice", Input{Name: "open-api", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com/v1"}}); err != nil {
		t.Fatal(err)
	}
	s3, err := s.Create(ctx, "alice", Input{Name: "lake", Type: TypeS3, Settings: map[string]any{"endpoint": "minio.internal:9000", "bucket": "lake", "access_key_id": "AK", "url_style": "path"}, Secret: SecretInput{Value: ptr("SK")}})
	if err != nil || s3.Settings["use_ssl"] != true || s3.Settings["region"] != "us-east-1" {
		t.Fatalf("s3: %+v %v", s3, err)
	}
}

func TestOwnersAndUpdates(t *testing.T) {
	ctx := context.Background()
	s, db, b := newService(t)
	v, err := s.Create(ctx, "alice", pg("crm", "pg.internal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "bob", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob reads alice's: %v", err)
	}
	if _, err := s.Update(ctx, "bob", v.ID, Patch{Name: ptr("mine")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob updates alice's: %v", err)
	}
	if err := s.Delete(ctx, "bob", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob deletes alice's: %v", err)
	}
	if list, _ := s.List(ctx, "bob"); len(list) != 0 {
		t.Fatalf("bob's list: %+v", list)
	}
	// Rotate the secret, rename; moving to a host not allowed is refused.
	if _, err := s.Update(ctx, "alice", v.ID, Patch{Name: ptr("crm-prod"), Secret: &SecretInput{Value: ptr("n3w")}}); err != nil {
		t.Fatal(err)
	}
	rec, _ := db.GetConnection(ctx, "alice", v.ID)
	if got, _ := b.Reveal(ctx, rec.SecretRef); string(got) != "n3w" || rec.Name != "crm-prod" {
		t.Fatalf("after rotation: %+v %q", rec, got)
	}
	if _, err := s.Update(ctx, "alice", v.ID, Patch{Settings: map[string]any{"host": "evil.com", "database": "d", "user": "u"}}); !isInvalid(err) {
		t.Fatalf("moved to a host not allowed: %v", err)
	}
	// Deleting removes the stored secret.
	if err := s.Delete(ctx, "alice", v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reveal(ctx, rec.SecretRef); err == nil {
		t.Fatal("the secret outlived its connection")
	}
}

func TestSpaceBindings(t *testing.T) {
	ctx := context.Background()
	s, db, _ := newService(t)
	now := time.Now()
	if err := db.CreateSpace(ctx, store.Space{ID: "s1", Owner: "alice", Name: "forecast", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	dev, _ := s.Create(ctx, "alice", pg("crm-dev", "pg-dev.internal"))
	prod, _ := s.Create(ctx, "alice", pg("crm-prod", "pg.internal"))
	bobs, _ := s.Create(ctx, "bob", pg("bobs", "pg.internal"))

	// By name or ID; the default must be given once there are several.
	in := SpaceInput{Default: "dev", Environments: map[string]map[string]string{"dev": {"crm": "crm-dev"}, "prod": {"crm": prod.ID}}}
	got, err := s.SetSpace(ctx, "alice", "s1", in)
	if err != nil || got.Default != "dev" || got.Environments["prod"]["crm"].Name != "crm-prod" || got.Environments["dev"]["crm"].ID != dev.ID {
		t.Fatalf("set: %+v %v", got, err)
	}
	if v, _ := s.Get(ctx, "alice", prod.ID); len(v.UsedBy) != 1 || v.UsedBy[0].SpaceName != "forecast" || v.UsedBy[0].Environment != "prod" {
		t.Fatalf("used by: %+v", v.UsedBy)
	}
	if err := s.Delete(ctx, "alice", prod.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete a bound connection: %v", err)
	}
	for name, bad := range map[string]SpaceInput{
		"different names":       {Default: "dev", Environments: map[string]map[string]string{"dev": {"crm": "crm-dev"}, "prod": {"billing": "crm-prod"}}},
		"no default":            {Environments: map[string]map[string]string{"dev": {"crm": "crm-dev"}, "prod": {"crm": "crm-prod"}}},
		"another's connection":  {Environments: map[string]map[string]string{"dev": {"crm": bobs.ID}}},
		"an unknown connection": {Environments: map[string]map[string]string{"dev": {"crm": "nope"}}},
		"a bad name":            {Environments: map[string]map[string]string{"dev": {"crm-x": "crm-dev"}}},
		"a bad environment":     {Environments: map[string]map[string]string{"Dev": {"crm": "crm-dev"}}},
	} {
		if _, err := s.SetSpace(ctx, "alice", "s1", bad); !isInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.SetSpace(ctx, "bob", "s1", SpaceInput{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob changes alice's space: %v", err)
	}
	if _, err := s.Space(ctx, "bob", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob reads alice's space: %v", err)
	}
	if cur, _ := s.Space(ctx, "alice", "s1"); len(cur.Environments) != 2 {
		t.Fatalf("a refused change changed something: %+v", cur)
	}
	// One environment: it's the default. None: everything unbound.
	if got, err := s.SetSpace(ctx, "alice", "s1", SpaceInput{Environments: map[string]map[string]string{"main": {"crm": "crm-dev"}}}); err != nil || got.Default != "main" {
		t.Fatalf("one environment: %+v %v", got, err)
	}
	if got, err := s.SetSpace(ctx, "alice", "s1", SpaceInput{}); err != nil || len(got.Environments) != 0 {
		t.Fatalf("cleared: %+v %v", got, err)
	}
	if err := s.Delete(ctx, "alice", prod.ID); err != nil {
		t.Fatalf("delete once unbound: %v", err)
	}
}
