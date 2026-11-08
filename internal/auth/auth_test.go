// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Low cost keeps tests fast; production hashes use bcrypt.DefaultCost.
func hash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func testUsers(t *testing.T) *Users {
	t.Helper()
	u, err := ParseUsers([]byte(fmt.Sprintf("users:\n  - username: alice\n    password_hash: %q\n  - username: bob\n    password_hash: %q\n",
		hash(t, "alice-password"), hash(t, "bob-password"))))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestVerify(t *testing.T) {
	u := testUsers(t)
	cases := []struct {
		user, pass string
		want       bool
	}{
		{"alice", "alice-password", true},
		{"bob", "bob-password", true},
		{"alice", "bob-password", false},
		{"alice", "", false},
		{"mallory", "alice-password", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := u.Verify(c.user, c.pass); got != c.want {
			t.Errorf("Verify(%q, %q) = %v, want %v", c.user, c.pass, got, c.want)
		}
	}
}

// Missing, wrong, and correct credentials → 401, 401, success.
func TestMiddleware(t *testing.T) {
	u := testUsers(t)
	h := (&Authenticator{Users: u}).Middleware(
		func(w http.ResponseWriter, _ *http.Request, _ Failure) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, _ := User(r.Context())
			_, _ = w.Write([]byte(name))
		}))

	do := func(set func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/spaces", nil)
		set(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	missing := do(func(*http.Request) {})
	wrong := do(func(r *http.Request) { r.SetBasicAuth("alice", "nope") })
	unknown := do(func(r *http.Request) { r.SetBasicAuth("mallory", "alice-password") })
	right := do(func(r *http.Request) { r.SetBasicAuth("alice", "alice-password") })

	for name, rec := range map[string]*httptest.ResponseRecorder{"missing": missing, "wrong": wrong, "unknown": unknown} {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s credentials: code %d, want 401", name, rec.Code)
		}
		if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("%s credentials: missing WWW-Authenticate challenge", name)
		}
	}
	if right.Code != http.StatusOK || right.Body.String() != "alice" {
		t.Fatalf("correct credentials: code %d body %q", right.Code, right.Body.String())
	}
}

func TestParseUsersRejectsBadFiles(t *testing.T) {
	good := hash(t, "password1")
	cases := map[string]string{
		"no users":       "users: []\n",
		"bad username":   fmt.Sprintf("users:\n  - username: Alice!\n    password_hash: %q\n", good),
		"duplicate":      fmt.Sprintf("users:\n  - username: alice\n    password_hash: %q\n  - username: alice\n    password_hash: %q\n", good, good),
		"plain password": "users:\n  - username: alice\n    password_hash: hunter2\n",
		"unknown key":    fmt.Sprintf("users:\n  - username: alice\n    password: x\n    password_hash: %q\n", good),
	}
	for name, body := range cases {
		if _, err := ParseUsers([]byte(body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestLoadUsersRefusesFileReadableByOthers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "users.yaml")
	body := fmt.Sprintf("users:\n  - username: alice\n    password_hash: %q\n", hash(t, "password1"))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil { // umask may have changed the mode
		t.Fatal(err)
	}
	if _, err := LoadUsers(p); err == nil || !strings.Contains(err.Error(), "chmod") {
		t.Fatalf("world-readable users file: got %v", err)
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	u, err := LoadUsers(p)
	if err != nil || u.Count() != 1 {
		t.Fatalf("mode 0640: %v", err)
	}
}

func TestHashPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password must be rejected")
	}
	h, err := HashPassword("long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("long-enough-password")) != nil {
		t.Fatal("hash does not verify")
	}
}

// Repeated good credentials skip bcrypt; wrong ones never do.
func TestVerifyCache(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("alice-password"), bcrypt.DefaultCost)
	u, err := ParseUsers([]byte(fmt.Sprintf("users:\n  - username: alice\n    password_hash: %q\n", h)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	u.cache.now = func() time.Time { return now }

	start := time.Now()
	if !u.Verify("alice", "alice-password") {
		t.Fatal("first check failed")
	}
	first := time.Since(start)
	start = time.Now()
	for i := 0; i < 100; i++ {
		if !u.Verify("alice", "alice-password") {
			t.Fatal("cached check failed")
		}
	}
	cached := time.Since(start) / 100
	if cached > 5*time.Millisecond || cached*10 > first {
		t.Fatalf("cached check took %v (first %v): cache not used", cached, first)
	}

	// Wrong passwords are never cached and never pass.
	for i := 0; i < 2; i++ {
		if u.Verify("alice", "wrong-password") {
			t.Fatal("wrong password passed")
		}
	}
	if len(u.cache.entries) != 1 {
		t.Fatalf("cache entries = %d, want only the good credentials", len(u.cache.entries))
	}
	// The cache holds no password material.
	for k := range u.cache.entries {
		if strings.Contains(string(k[:]), "alice-password") {
			t.Fatal("password stored in cache key")
		}
	}

	// After the TTL, bcrypt runs again (and still accepts).
	now = now.Add(cacheTTL + time.Second)
	if !u.Verify("alice", "alice-password") {
		t.Fatal("check after expiry failed")
	}
	// Full cache is cleared rather than growing without bound.
	for i := 0; i < cacheMaxEntries; i++ {
		u.cache.add([32]byte{byte(i), byte(i >> 8)})
	}
	if len(u.cache.entries) > cacheMaxEntries {
		t.Fatalf("cache grew to %d", len(u.cache.entries))
	}
}

// The repository's demo users are recognised (nbsandboxd warns about them).
func TestDemoUsers(t *testing.T) {
	b, err := os.ReadFile("../../config/users.yaml")
	if err != nil {
		t.Fatal(err)
	}
	u, err := ParseUsers(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.DemoUsers(); !slices.Equal(got, []string{"alice", "bob"}) {
		t.Fatalf("demo users %v", got)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("a real password"), bcrypt.MinCost)
	u, _ = ParseUsers([]byte("users:\n  - username: carol\n    password_hash: \"" + string(hash) + "\"\n"))
	if got := u.DemoUsers(); len(got) != 0 {
		t.Fatalf("demo users %v", got)
	}
}
