// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package authtest is a fake OpenID Connect provider for tests: it serves a
// discovery document and a key set over a local test server and mints
// signed access tokens.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
)

// Audience is the audience tokens are minted for, and Config expects.
const Audience = "api://nbsandbox-test"

// IDP is a running fake provider.
type IDP struct {
	Server *httptest.Server
	key    jose.JSONWebKey
}

// New starts a provider that stops when the test ends.
func New(t testing.TB) *IDP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &IDP{key: jose.JSONWebKey{Key: k, KeyID: "test-key", Algorithm: string(jose.RS256), Use: "sig"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.Server.URL, "jwks_uri": p.Server.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{p.key.Public()}})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Server.Close)
	return p
}

// Config is a matching auth.OIDCConfig: callers are "test:<sub>".
func (p *IDP) Config() auth.OIDCConfig {
	return auth.OIDCConfig{Issuer: p.Server.URL, Audience: Audience, IDPrefix: "test", IDClaims: []string{"sub"}}
}

// Verifier returns a verifier for this provider.
func (p *IDP) Verifier(t testing.TB) *auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier(p.Config(), p.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Token mints a token for subject with the given scopes (in "scp"), valid for
// ten minutes. extra claims are added or, when nil, removed.
func (p *IDP) Token(t testing.TB, subject string, scopes []string, extra map[string]any) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{
		"iss": p.Server.URL, "aud": Audience, "sub": subject, "azp": "test-client",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"scp": strings.Join(scopes, " "),
	}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("at+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	jws, err := s.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
