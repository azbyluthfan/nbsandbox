// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/gateway"
	"github.com/azbyluthfan/nbsandbox/internal/secrets"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

var gwKey = []byte(strings.Repeat("g", 32))

func TestResolveAndFetcher(t *testing.T) {
	ctx := context.Background()
	s, db, _ := newService(t)
	s.d.Fetcher = FetcherSettings{Image: "fetcher:1", GatewayAddress: "nbsandbox-gateway:3128", GatewayContainer: "nbsandbox-gateway", GatewayKey: gwKey}
	now := time.Now()
	for _, id := range []string{"s1", "s2"} {
		if err := db.CreateSpace(ctx, store.Space{ID: id, Owner: "alice", Name: id, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create(ctx, "alice", pg("crm-dev", "pg-dev.internal")); err != nil {
		t.Fatal(err)
	}
	prod, _ := s.Create(ctx, "alice", pg("crm-prod", "pg.internal"))
	lake, err := s.Create(ctx, "alice", Input{Name: "lake", Type: TypeS3, Settings: map[string]any{
		"endpoint": "minio.internal:9000", "bucket": "data", "access_key_id": "AK", "use_ssl": false, "url_style": "path"},
		Secret: SecretInput{Value: ptr("s3-SECRET")}})
	if err != nil {
		t.Fatal(err)
	}
	aws, _ := s.Create(ctx, "alice", Input{Name: "aws", Type: TypeS3, Settings: map[string]any{
		"endpoint": "s3.eu-west-1.amazonaws.internal", "bucket": "logs"}, Secret: SecretInput{None: true}})
	api, _ := s.Create(ctx, "alice", Input{Name: "api", Type: TypeHTTP, Settings: map[string]any{"url": "https://api.example.com"},
		Secret: SecretInput{Value: ptr("tok")}})
	if _, err := s.SetSpace(ctx, "alice", "s1", SpaceInput{Default: "dev", Environments: map[string]map[string]string{
		"dev":  {"crm": "crm-dev", "lake": lake.ID, "logs": aws.ID, "api": api.ID},
		"prod": {"crm": prod.ID, "lake": lake.ID, "logs": aws.ID, "api": api.ID},
	}}); err != nil {
		t.Fatal(err)
	}

	// Resolve: the default, a named environment, refusals.
	env, names, err := s.Resolve(ctx, "s1", "")
	if err != nil || env != "dev" || len(names) != 4 {
		t.Fatalf("default: %q %v %v", env, names, err)
	}
	if env, names, err = s.Resolve(ctx, "s1", "prod"); err != nil || env != "prod" || names["crm"] != prod.ID {
		t.Fatalf("prod: %q %v %v", env, names, err)
	}
	var inv *spaces.InvalidError
	if _, _, err := s.Resolve(ctx, "s1", "staging"); !errors.As(err, &inv) || !strings.Contains(err.Error(), "dev, prod") {
		t.Fatalf("unknown environment: %v", err)
	}
	if env, names, err := s.Resolve(ctx, "s2", ""); err != nil || env != "" || names != nil {
		t.Fatalf("a space binding nothing: %q %v %v", env, names, err)
	}
	if _, _, err := s.Resolve(ctx, "s2", "prod"); !errors.As(err, &inv) {
		t.Fatalf("an environment where there are none: %v", err)
	}

	// Fetcher: credentials for the fetcher; a token for exactly its endpoints.
	exp := now.Add(time.Hour)
	f, err := s.Fetcher(ctx, "run1", "alice", names, exp)
	if err != nil || f == nil {
		t.Fatalf("fetcher: %v", err)
	}
	if f.Image != "fetcher:1" || f.Gateway != "nbsandbox-gateway" || len(f.Token) != 64 {
		t.Fatalf("fetcher: %+v", f)
	}
	var cfg fetcherConfig
	if err := json.Unmarshal(f.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	var got []string
	secretsOf := map[string]string{}
	for _, src := range cfg.Sources {
		got = append(got, src.Name)
		secretsOf[src.Name] = src.Secret
	}
	if !reflect.DeepEqual(got, []string{"crm", "lake", "logs"}) { // sorted; http isn't the fetcher's
		t.Fatalf("sources: %v", got)
	}
	if secretsOf["crm"] != "pa55" || secretsOf["lake"] != "s3-SECRET" || secretsOf["logs"] != "" || cfg.Token != f.Token {
		t.Fatalf("secrets: %v", secretsOf)
	}
	c, err := gateway.Verify(gwKey, cfg.Gateway.Token, now)
	if err != nil || c.Run != "run1" || c.Exp != exp.Unix() || cfg.Gateway.Address != "nbsandbox-gateway:3128" {
		t.Fatalf("gateway token: %+v %v", c, err)
	}
	want := []string{"pg.internal:5432", "minio.internal:9000", "logs.s3.eu-west-1.amazonaws.internal:443"}
	if !reflect.DeepEqual(c.Allow, want) {
		t.Fatalf("allowed endpoints: %v, want %v", c.Allow, want)
	}

	// Nothing the fetcher reads: no fetcher.
	if f, err := s.Fetcher(ctx, "run2", "alice", map[string]string{"api": api.ID}, exp); err != nil || f != nil {
		t.Fatalf("http only: %+v %v", f, err)
	}
	// Someone else's run, a connection gone, an endpoint no longer allowed.
	if _, err := s.Fetcher(ctx, "run3", "bob", names, exp); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("another owner: %v", err)
	}
	if _, err := s.Fetcher(ctx, "run3", "alice", map[string]string{"crm": "gone"}, exp); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("deleted: %v", err)
	}
	s.d.Allowed = NewAllowed([]string{"minio.internal"})
	if _, err := s.Fetcher(ctx, "run3", "alice", names, exp); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no longer allows") {
		t.Fatalf("no longer allowed: %v", err)
	}
}

// mountProvider is a provider whose values the platform mounts (Kubernetes).
type mountProvider struct{ stored map[string]string }

func (m *mountProvider) Name() string { return "kubernetes" }
func (m *mountProvider) Store(_ context.Context, id string, v []byte) (string, error) {
	m.stored[id] = string(v)
	return "nbsandbox-conn-" + id + "#secret", nil
}
func (m *mountProvider) Remove(context.Context, string) error { return nil }
func (m *mountProvider) Check(context.Context, string) error  { return nil }
func (m *mountProvider) Mount(ref string) (string, string, error) {
	name, key, _ := strings.Cut(ref, "#")
	return name, key, nil
}

func TestFetcherMountsKubernetesSecrets(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	s := New(Deps{DB: db, Secrets: &mountProvider{stored: map[string]string{}}, Allowed: NewAllowed([]string{"*.internal"}),
		Fetcher: FetcherSettings{Image: "f", GatewayAddress: "gw.svc:3128", GatewayContainer: "gw.svc:3128", GatewayKey: gwKey}})
	crm, err := s.Create(ctx, "alice", pg("crm", "pg.internal"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Fetcher(ctx, "r1", "alice", map[string]string{"crm": crm.ID}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(f.Config), "pa55") {
		t.Fatal("nbsandboxd put the credential in the configuration")
	}
	var cfg fetcherConfig
	_ = json.Unmarshal(f.Config, &cfg)
	if len(f.Mounts) != 1 || f.Mounts[0].Secret != "nbsandbox-conn-"+crm.ID || f.Mounts[0].Key != "secret" ||
		cfg.Sources[0].SecretFile != "/etc/nbsandbox-fetcher/secrets/"+f.Mounts[0].Path || f.Gateway != "gw.svc:3128" {
		t.Fatalf("mounts %+v, source %+v", f.Mounts, cfg.Sources[0])
	}
}

func TestFetcherReadsVault(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVaultServer{}
	srv := httptest.NewServer(fv)
	defer srv.Close()
	vp, err := secrets.NewVault(secrets.VaultConfig{Address: srv.URL, Token: "t", References: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Deps{DB: storetest.Open(t), Secrets: vp, Allowed: NewAllowed([]string{"*.internal"}),
		Fetcher: FetcherSettings{Image: "f", GatewayAddress: "gw:3128", GatewayKey: gwKey, ServiceAccount: "nbsandbox-fetcher", FetcherRole: "fetchers"}})
	crm, err := s.Create(ctx, "alice", pg("crm", "pg.internal"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Fetcher(ctx, "r1", "alice", map[string]string{"crm": crm.ID}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var cfg fetcherConfig
	_ = json.Unmarshal(f.Config, &cfg)
	src := cfg.Sources[0]
	if strings.Contains(string(f.Config), "pa55") || src.SecretVault == nil || src.SecretVault.Path != "nbsandbox/connections/conn-"+crm.ID ||
		src.SecretVault.Key != "secret" || cfg.Vault == nil || cfg.Vault.Role != "fetchers" || !f.VaultLogin || f.ServiceAccount != "nbsandbox-fetcher" {
		t.Fatalf("config %s, fetcher %+v", f.Config, f)
	}
	c, _ := gateway.Verify(gwKey, cfg.Gateway.Token, time.Now())
	if !slices.Contains(c.Allow, hostPort(srv.URL)) {
		t.Fatalf("vault not in the token: %v", c.Allow)
	}
}

// fakeVaultServer accepts KV v2 writes, for creating connections.
type fakeVaultServer struct{}

func (fakeVaultServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"data":{"version":1}}`)
}
