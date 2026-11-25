// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeVault is a KV v2 mount "secret" and a Kubernetes auth method.
type fakeVault struct {
	mu     sync.Mutex
	data   map[string]map[string]string // path -> data
	logins int
	reads  []string // data reads: nbsandboxd must never make one
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	if p == "/v1/auth/kubernetes/login" {
		var b struct{ Role, JWT string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Role != "nbsandbox" || b.JWT != "sa-jwt" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"errors":["permission denied"]}`)
			return
		}
		f.logins++
		_, _ = io.WriteString(w, `{"auth":{"client_token":"t-1","lease_duration":3600}}`)
		return
	}
	if r.Header.Get("X-Vault-Token") != "t-1" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch {
	case strings.HasPrefix(p, "/v1/secret/data/") && r.Method == http.MethodPost:
		var b struct{ Data map[string]string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.data[strings.TrimPrefix(p, "/v1/secret/data/")] = b.Data
		_, _ = io.WriteString(w, `{"data":{"version":1}}`)
	case strings.HasPrefix(p, "/v1/secret/data/"):
		f.reads = append(f.reads, p)
		w.WriteHeader(http.StatusTeapot)
	case strings.HasPrefix(p, "/v1/secret/metadata/"):
		key := strings.TrimPrefix(p, "/v1/secret/metadata/")
		if _, ok := f.data[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.data, key)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"current_version":1}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestVault(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{data: map[string]map[string]string{"team/crm": {"password": "theirs"}}}
	srv := httptest.NewServer(fv)
	defer srv.Close()
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVault(VaultConfig{Address: "vault:8200", Role: "r"}); err == nil {
		t.Fatal("an address without a scheme was accepted")
	}
	if _, err := NewVault(VaultConfig{Address: srv.URL}); err == nil {
		t.Fatal("neither a role nor a token")
	}
	v, err := NewVault(VaultConfig{Address: srv.URL, Role: "nbsandbox", TokenFile: jwt, References: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	ref, err := v.Store(ctx, "c1", []byte("pa55"))
	if err != nil || ref != "nbsandbox/connections/conn-c1#secret" || fv.data["nbsandbox/connections/conn-c1"]["secret"] != "pa55" {
		t.Fatalf("store: %q %v %v", ref, err, fv.data)
	}
	if _, err := v.Store(ctx, "c2", []byte("x")); err != nil || fv.logins != 1 {
		t.Fatalf("the token wasn't kept: %d logins, %v", fv.logins, err)
	}

	// References: under an allowed prefix, existing; never nbsandbox's own.
	if err := v.Check(ctx, "team/crm#password"); err != nil {
		t.Fatalf("a good reference: %v", err)
	}
	for _, bad := range []string{"team/missing#k", "other/crm#k", "nbsandbox/connections/conn-c1#secret", "team/../x#k", "team/crm", "team/crm#", "/team/crm#k"} {
		if err := v.Check(ctx, bad); !errors.Is(err, ErrBadReference) {
			t.Errorf("Check(%q): %v", bad, err)
		}
	}

	// Remove: only its own.
	if err := v.Remove(ctx, "team/crm#password"); err != nil || fv.data["team/crm"] == nil {
		t.Fatalf("removed a referenced secret: %v", err)
	}
	if err := v.Remove(ctx, ref); err != nil || fv.data["nbsandbox/connections/conn-c1"] != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := v.Remove(ctx, ref); err != nil {
		t.Fatalf("removing twice: %v", err)
	}
	if len(fv.reads) != 0 {
		t.Fatalf("nbsandboxd read a value: %v", fv.reads)
	}

	// A refused login says why, without the token.
	bad, _ := NewVault(VaultConfig{Address: srv.URL, Role: "other", TokenFile: jwt})
	if _, err := bad.Store(ctx, "c3", []byte("x")); err == nil || !strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "sa-jwt") {
		t.Fatalf("refused login: %v", err)
	}
	// A fixed token skips the login.
	tv, _ := NewVault(VaultConfig{Address: srv.URL, Token: "t-1"})
	if _, err := tv.Store(ctx, "c4", []byte("x")); err != nil {
		t.Fatalf("token: %v", err)
	}
}
