// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func limiter(f int, c *clock) *LoginLimiter {
	l := NewLoginLimiter(f, time.Minute)
	l.now = c.now
	return l
}

func TestLoginLimiterAllowanceAndRefill(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	l := limiter(3, c)
	for i := range 3 {
		if err := l.Check("user:a"); err != nil {
			t.Fatalf("attempt %d refused: %v", i, err)
		}
		l.Fail("user:a")
	}
	err := l.Check("user:a")
	wait, limited := IsTooManyAttempts(err)
	if !limited || wait < 59*time.Second || wait > 62*time.Second {
		t.Fatalf("after 3 failures: %v (retry after %v)", err, wait)
	}
	if err := l.Check("user:b"); err != nil {
		t.Errorf("another user refused: %v", err)
	}
	c.add(time.Minute) // one more failure allowed
	if err := l.Check("user:a"); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
	l.Fail("user:a")
	if err := l.Check("user:a"); err == nil {
		t.Fatal("refilled one, used it: should refuse again")
	}
	l.Reset("user:a")
	if err := l.Check("user:a"); err != nil {
		t.Fatalf("after a successful sign-in: %v", err)
	}
	if NewLoginLimiter(0, time.Minute) != nil {
		t.Error("0 failures turns the limit off")
	}
	var off *LoginLimiter
	off.Fail("x")
	off.Reset("x")
	if off.Check("x") != nil {
		t.Error("a nil limiter allows everything")
	}
}

func TestLoginLimiterForgetsRefilledKeys(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	l := limiter(2, c)
	for i := range 100 {
		l.Fail(fmt.Sprintf("addr:10.0.0.%d", i))
	}
	c.add(3 * time.Minute) // every key is full again
	l.Fail("addr:new")     // sweeps
	if n := len(l.buckets); n != 1 {
		t.Errorf("%d keys kept, want only the new one", n)
	}
}

func TestClientIP(t *testing.T) {
	proxies, err := NewTrustedProxies([]string{"10.0.0.0/8", "192.168.1.5"})
	if err != nil {
		t.Fatal(err)
	}
	h := func(v ...string) http.Header { return http.Header{"X-Forwarded-For": v} }
	for name, tc := range map[string]struct {
		remote  string
		header  http.Header
		trusted *TrustedProxies
		want    string
	}{
		"no proxies: the peer, header ignored":   {"203.0.113.9:5000", h("1.2.3.4"), nil, "203.0.113.9"},
		"untrusted peer: header ignored":         {"203.0.113.9:5000", h("1.2.3.4"), proxies, "203.0.113.9"},
		"trusted proxy: the client it saw":       {"10.1.2.3:5000", h("198.51.100.7"), proxies, "198.51.100.7"},
		"spoofed hops before the client ignored": {"10.1.2.3:5000", h("6.6.6.6, 198.51.100.7"), proxies, "198.51.100.7"},
		"through two proxies":                    {"10.1.2.3:5000", h("198.51.100.7, 192.168.1.5"), proxies, "198.51.100.7"},
		"several header lines":                   {"10.1.2.3:5000", h("6.6.6.6", "198.51.100.7"), proxies, "198.51.100.7"},
		"malformed header: the peer":             {"10.1.2.3:5000", h("not-an-ip"), proxies, "10.1.2.3"},
		"no header from a proxy: the proxy":      {"10.1.2.3:5000", nil, proxies, "10.1.2.3"},
		"IPv6 peer":                              {"[2001:db8::1]:443", nil, nil, "2001:db8::1"},
	} {
		if got := ClientIP(context.Background(), tc.remote, tc.header, tc.trusted); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	if _, err := NewTrustedProxies([]string{"not an address!"}); err == nil {
		t.Error("a bad proxy entry must be refused")
	}
	if p, err := NewTrustedProxies(nil); p != nil || err != nil {
		t.Error("no proxies: nil")
	}
}

// A host name (a Compose service) is looked up, and again after the
// refresh: a restarted proxy container may have a new address.
func TestTrustedProxiesByHostName(t *testing.T) {
	p, err := NewTrustedProxies([]string{"caddy"})
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddr("172.18.0.5")
	lookups := 0
	p.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		lookups++
		if host != "caddy" {
			t.Fatalf("looked up %q", host)
		}
		return []netip.Addr{addr}, nil
	}
	p.refresh = time.Hour
	ctx := context.Background()
	if !p.Contains(ctx, netip.MustParseAddr("172.18.0.5")) || p.Contains(ctx, netip.MustParseAddr("172.18.0.6")) {
		t.Fatal("caddy's address")
	}
	p.Contains(ctx, addr)
	if lookups != 1 {
		t.Errorf("%d lookups within the refresh, want 1", lookups)
	}
	addr = netip.MustParseAddr("172.18.0.9") // caddy restarted
	p.refresh = 0
	if !p.Contains(ctx, netip.MustParseAddr("172.18.0.9")) {
		t.Error("the new address after a refresh")
	}
	if got := ClientIP(context.Background(), "172.18.0.9:5000", http.Header{"X-Forwarded-For": {"198.51.100.7"}}, p); got != "198.51.100.7" {
		t.Errorf("through caddy: %q", got)
	}
}

// Password guessing gets 429 with Retry-After, per username and per client
// address, without the password being checked; a correct password restores
// the username.
func TestMiddlewareLimitsFailedSignIns(t *testing.T) {
	users, err := ParseUsers([]byte("users:\n  - username: alice\n    password_hash: " + mustHash(t, "alice-password") +
		"\n  - username: bob\n    password_hash: " + mustHash(t, "bob-password") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	a := &Authenticator{Users: users, Limiter: limiter(3, c), LimitByAddress: true}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := a.Middleware(func(w http.ResponseWriter, _ *http.Request, f Failure) { w.WriteHeader(f.Status) }, ok)
	try := func(user, pass, from string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/spaces", nil)
		r.RemoteAddr = from + ":4000"
		r.SetBasicAuth(user, pass)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	for range 3 {
		if rec := try("alice", "guess", "198.51.100.1"); rec.Code != 401 {
			t.Fatalf("wrong password: %d", rec.Code)
		}
	}
	rec := try("alice", "alice-password", "198.51.100.2") // right password, another address: still refused
	if rec.Code != 429 {
		t.Fatalf("after 3 failures: %d, want 429", rec.Code)
	}
	if s, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || s < 1 {
		t.Errorf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	if rec := try("bob", "bob-password", "198.51.100.3"); rec.Code != 204 {
		t.Errorf("another user from another address: %d", rec.Code)
	}
	// The guessing address is limited for every username (password spraying).
	if rec := try("bob", "bob-password", "198.51.100.1"); rec.Code != 429 {
		t.Errorf("another user from the guessing address: %d, want 429", rec.Code)
	}
	c.add(time.Minute)
	if rec := try("alice", "alice-password", "198.51.100.2"); rec.Code != 204 {
		t.Fatalf("after the refill: %d", rec.Code)
	}
	for range 3 { // the success restored alice's whole allowance
		if rec := try("alice", "guess", "198.51.100.4"); rec.Code != 401 {
			t.Fatalf("after a success, failures are counted afresh: %d", rec.Code)
		}
	}
	if !errorsIsTooMany(a, "alice", "x") {
		t.Error("Authenticate must report too many attempts to gRPC too")
	}
}

func errorsIsTooMany(a *Authenticator, user, pass string) bool {
	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth(user, pass)
	_, err := a.Authenticate(WithClientAddr(context.Background(), "198.51.100.9"), r.Header.Get("Authorization"))
	_, limited := IsTooManyAttempts(err)
	return limited && FailureMessage(err) != "valid credentials required"
}

// Without LimitByAddress (behind an unnamed proxy), one guesser can't lock
// out other users who share the proxy's address.
func TestNoAddressLimitWithoutTrustedAddresses(t *testing.T) {
	users, err := ParseUsers([]byte("users:\n  - username: bob\n    password_hash: " + mustHash(t, "bob-password") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	a := &Authenticator{Users: users, Limiter: limiter(2, c)}
	ctx := WithClientAddr(context.Background(), "10.0.0.2") // the proxy
	basic := func(u, p string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetBasicAuth(u, p)
		return r.Header.Get("Authorization")
	}
	for range 5 {
		_, _ = a.Authenticate(ctx, basic("mallory", "guess"))
	}
	if _, err := a.Authenticate(ctx, basic("bob", "bob-password")); err != nil {
		t.Fatalf("bob locked out by another user's guesses through the same proxy: %v", err)
	}
}
