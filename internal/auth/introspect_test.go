// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// introspection is a fake RFC 7662 endpoint: tokens in revoked are
// inactive; status, if set, is returned instead of an answer.
type introspection struct {
	srv     *httptest.Server
	calls   atomic.Int32
	revoked map[string]bool
	status  atomic.Int32
}

func newIntrospection(t *testing.T) *introspection {
	in := &introspection{revoked: map[string]bool{}}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.calls.Add(1)
		if id, secret, _ := r.BasicAuth(); id != "nbsandbox" || secret != "s3cret" {
			http.Error(w, "unauthorized client", http.StatusUnauthorized)
			return
		}
		if s := in.status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		_ = r.ParseForm()
		_ = json.NewEncoder(w).Encode(map[string]bool{"active": !in.revoked[r.PostForm.Get("token")]})
	}))
	t.Cleanup(in.srv.Close)
	return in
}

func introspected(t *testing.T, p *idp, in *introspection, mod func(*IntrospectionConfig)) *Verifier {
	t.Helper()
	return p.verifier(t, func(c *OIDCConfig) {
		c.Introspection = &IntrospectionConfig{URL: in.srv.URL, ClientID: "nbsandbox", ClientSecret: "s3cret"}
		if mod != nil {
			mod(c.Introspection)
		}
	})
}

func TestIntrospectionRevokedTokenIsRefused(t *testing.T) {
	p, in := newIDP(t), newIntrospection(t)
	v := introspected(t, p, in, nil)
	tok := p.token(t, nil)
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("active token: %v", err)
	}
	if _, err := v.Verify(context.Background(), tok); err != nil || in.calls.Load() != 1 {
		t.Fatalf("second use within the cache: %v, %d calls (want 1)", err, in.calls.Load())
	}
	in.revoked[tok] = true
	// Within the cache the old answer stands; after it, the revocation shows.
	v.now = func() time.Time { return testNow.Add(31 * time.Second) }
	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrInvalidToken) || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked token: %v", err)
	}
	// An inactive answer is kept until the token expires: no more calls.
	n := in.calls.Load()
	v.now = func() time.Time { return testNow.Add(10 * time.Minute) }
	if _, err := v.Verify(context.Background(), tok); err == nil || in.calls.Load() != n {
		t.Errorf("a revoked token stays refused without asking again: %v, %d calls", err, in.calls.Load()-n)
	}
}

func TestIntrospectionFailsClosed(t *testing.T) {
	p, in := newIDP(t), newIntrospection(t)
	v := introspected(t, p, in, nil)
	in.status.Store(http.StatusServiceUnavailable)
	if _, err := v.Verify(context.Background(), p.token(t, nil)); !errors.Is(err, ErrInvalidToken) ||
		!strings.Contains(err.Error(), "introspection failed") {
		t.Fatalf("provider down: %v (a token must not be accepted without an answer)", err)
	}
	// Our client's credentials wrong: refused too.
	in.status.Store(0)
	v = introspected(t, p, in, func(c *IntrospectionConfig) { c.ClientSecret = "wrong" })
	if _, err := v.Verify(context.Background(), p.token(t, nil)); err == nil {
		t.Fatal("introspection with wrong client credentials must refuse")
	}
}

func TestIntrospectionOnlyForTokensThatPassLocalChecks(t *testing.T) {
	p, in := newIDP(t), newIntrospection(t)
	v := introspected(t, p, in, nil)
	if _, err := v.Verify(context.Background(), p.token(t, map[string]any{"aud": "someone-else"})); err == nil {
		t.Fatal("wrong audience accepted")
	}
	if in.calls.Load() != 0 {
		t.Errorf("a token that fails locally was sent to the provider (%d calls)", in.calls.Load())
	}
}

func TestIntrospectionEndpointFromDiscovery(t *testing.T) {
	p, in := newIDP(t), newIntrospection(t)
	// The fake provider's discovery document names no introspection
	// endpoint: the error says to configure it.
	v := introspected(t, p, in, func(c *IntrospectionConfig) { c.URL = "" })
	if _, err := v.Verify(context.Background(), p.token(t, nil)); err == nil || !strings.Contains(err.Error(), "introspection_endpoint") {
		t.Fatalf("no endpoint: %v", err)
	}
}

func TestIntrospectionConfigValidate(t *testing.T) {
	base := OIDCConfig{Issuer: "https://idp.example", Audience: "nbsandbox", IDPrefix: "kc", IDClaims: []string{"sub"}}
	for name, c := range map[string]IntrospectionConfig{
		"no client":      {ClientSecret: "x"},
		"no secret":      {ClientID: "x"},
		"plain http":     {URL: "http://idp.example/introspect", ClientID: "x", ClientSecret: "y"},
		"cache too long": {ClientID: "x", ClientSecret: "y", Cache: time.Hour},
	} {
		cfg := base
		cfg.Introspection = &c
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg := base
	cfg.Introspection = &IntrospectionConfig{ClientID: "x", ClientSecret: "y", Cache: time.Minute}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid: %v", err)
	}
}
