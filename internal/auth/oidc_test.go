// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// idp is a fake identity provider: discovery, a key set, and token minting.
type idp struct {
	srv     *httptest.Server
	issuer  string
	keys    []jose.JSONWebKey // private keys; the set serves their public halves
	fetches atomic.Int32
	docIss  string // issuer named in the discovery document (default: issuer)
}

func newIDP(t testing.TB) *idp {
	t.Helper()
	p := &idp{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := p.docIss
		if iss == "" {
			iss = p.issuer
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "jwks_uri": p.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		p.fetches.Add(1)
		var set jose.JSONWebKeySet
		for _, k := range p.keys {
			set.Keys = append(set.Keys, k.Public())
		}
		_ = json.NewEncoder(w).Encode(set)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	p.issuer = p.srv.URL
	p.addRSA(t, "k1")
	return p
}

func (p *idp) addRSA(t testing.TB, kid string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.keys = append(p.keys, jose.JSONWebKey{Key: k, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"})
}

func (p *idp) addEC(t testing.TB, kid string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.keys = append(p.keys, jose.JSONWebKey{Key: k, KeyID: kid, Algorithm: string(jose.ES256), Use: "sig"})
}

// sign signs claims with the key kid (as named in the header).
func sign(t testing.TB, key jose.JSONWebKey, alg jose.SignatureAlgorithm, claims map[string]any, opts *jose.SignerOptions) string {
	t.Helper()
	if opts == nil {
		opts = &jose.SignerOptions{}
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts.WithType("at+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
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

var testNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func (p *idp) claims(extra map[string]any) map[string]any {
	c := map[string]any{
		"iss": p.issuer, "aud": "api://nbsandbox", "sub": "user-1", "tid": "tenant-1", "oid": "object-1",
		"iat": testNow.Unix(), "nbf": testNow.Unix(), "exp": testNow.Add(time.Hour).Unix(),
		"scp": "nbsandbox.read nbsandbox.runs", "azp": "client-app", "name": "Ada",
	}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func (p *idp) token(t testing.TB, extra map[string]any) string {
	return sign(t, p.keys[0], jose.RS256, p.claims(extra), nil)
}

func (p *idp) verifier(t testing.TB, mod func(*OIDCConfig)) *Verifier {
	t.Helper()
	cfg := OIDCConfig{Issuer: p.issuer, Audience: "api://nbsandbox", IDPrefix: "test", IDClaims: []string{"sub"}}
	if mod != nil {
		mod(&cfg)
	}
	v, err := NewVerifier(cfg, p.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return testNow }
	return v
}

func TestVerifyValidToken(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, nil)
	got, err := v.Verify(context.Background(), p.token(t, map[string]any{"roles": []any{"nbsandbox.functions.invoke"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "test:user-1" || got.Method != "oidc" || got.Client != "client-app" || got.Name != "Ada" {
		t.Fatalf("principal %+v", got)
	}
	for _, s := range []string{ScopeRead, ScopeRuns, ScopeFunctionsInvoke} {
		if !got.Has(s) {
			t.Errorf("missing scope %s (scp and roles both count)", s)
		}
	}
	if got.Has(ScopeFilesWrite) || got.Has(ScopeFunctionsPublish) {
		t.Fatal("a scope the token doesn't carry must not be granted")
	}
}

func TestVerifyEntraStyleIDAndAudienceList(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, func(c *OIDCConfig) { c.IDPrefix = "entra"; c.IDClaims = []string{"tid", "oid"} })
	got, err := v.Verify(context.Background(), p.token(t, map[string]any{"aud": []any{"other", "api://nbsandbox"}, "scp": nil, "scope": "nbsandbox.read"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "entra:tenant-1:object-1" || !got.Has(ScopeRead) {
		t.Fatalf("principal %+v scopes %v", got, got.Scopes())
	}
}

func TestVerifyECKeyAndRotation(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, nil)
	if _, err := v.Verify(context.Background(), p.token(t, nil)); err != nil {
		t.Fatal(err)
	}
	// The provider rotates in a new key: a token with its (unknown) kid
	// triggers a refetch, once the one-minute gap since the last fetch has
	// passed. Providers publish keys well before signing with them.
	p.addEC(t, "k2")
	v.now = func() time.Time { return testNow.Add(61 * time.Second) }
	tok := sign(t, p.keys[1], jose.ES256, p.claims(nil), nil)
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("token signed with a newly published key: %v", err)
	}
	if n := p.fetches.Load(); n != 2 {
		t.Fatalf("key set fetched %d times, want 2 (once, then once for the new key)", n)
	}
}

func TestVerifyUnknownKeyFetchesAtMostOncePerMinute(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, nil)
	stranger := jose.JSONWebKey{}
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	stranger.Key, stranger.KeyID = k, "made-up"
	for range 20 {
		if _, err := v.Verify(context.Background(), sign(t, stranger, jose.RS256, p.claims(nil), nil)); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("token from an unknown key: %v", err)
		}
	}
	if n := p.fetches.Load(); n != 1 {
		t.Fatalf("key set fetched %d times for 20 unknown-key tokens, want 1", n)
	}
	v.now = func() time.Time { return testNow.Add(61 * time.Second) }
	_, _ = v.Verify(context.Background(), sign(t, stranger, jose.RS256, p.claims(nil), nil))
	if n := p.fetches.Load(); n != 2 {
		t.Fatalf("after a minute: fetched %d times, want 2", n)
	}
}

func TestVerifyRefusals(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, nil)
	attacker, _ := rsa.GenerateKey(rand.Reader, 2048)
	hmacKey := jose.JSONWebKey{Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "k1"}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	payload, _ := json.Marshal(p.claims(nil))
	none := enc(`{"alg":"none","typ":"at+jwt"}`) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."

	cases := map[string]string{
		"wrong issuer":          p.token(t, map[string]any{"iss": "https://evil.example"}),
		"wrong audience":        p.token(t, map[string]any{"aud": "api://other"}),
		"no audience":           p.token(t, map[string]any{"aud": nil}),
		"expired":               p.token(t, map[string]any{"exp": testNow.Add(-2 * time.Minute).Unix()}),
		"no exp":                p.token(t, map[string]any{"exp": nil}),
		"not yet valid":         p.token(t, map[string]any{"nbf": testNow.Add(5 * time.Minute).Unix()}),
		"issued in the future":  p.token(t, map[string]any{"iat": testNow.Add(5 * time.Minute).Unix()}),
		"lives too long":        p.token(t, map[string]any{"exp": testNow.Add(48 * time.Hour).Unix()}),
		"no subject":            p.token(t, map[string]any{"sub": nil}),
		"subject with a colon":  p.token(t, map[string]any{"sub": "a:b"}),
		"alg none":              none,
		"HMAC signed":           sign(t, hmacKey, jose.HS256, p.claims(nil), nil),
		"signed by another key": sign(t, jose.JSONWebKey{Key: attacker, KeyID: "k1"}, jose.RS256, p.claims(nil), nil),
		// The attacker embeds their own public key: it must be ignored.
		"embedded jwk": sign(t, jose.JSONWebKey{Key: attacker, KeyID: "k1"}, jose.RS256, p.claims(nil), &jose.SignerOptions{EmbedJWK: true}),
		"not a JWT":    "hello",
		"too long":     strings.Repeat("a", maxTokenBytes+1),
		"tampered":     tamper(p.token(t, nil)),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if p, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("accepted (%+v) or wrong error: %v", p, err)
			}
		})
	}
	// Within the leeway is fine.
	if _, err := v.Verify(context.Background(), p.token(t, map[string]any{"exp": testNow.Add(-30 * time.Second).Unix()})); err != nil {
		t.Fatalf("expired 30s ago, leeway 60s: %v", err)
	}
}

// tamper changes the payload's subject while keeping the signature.
func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	b = []byte(strings.Replace(string(b), `"user-1"`, `"admin1"`, 1))
	parts[1] = base64.RawURLEncoding.EncodeToString(b)
	return strings.Join(parts, ".")
}

func TestVerifyDiscoveryIssuerMismatch(t *testing.T) {
	p := newIDP(t)
	p.docIss = "https://someone-else.example"
	v := p.verifier(t, nil)
	if _, err := v.Verify(context.Background(), p.token(t, nil)); !errors.Is(err, ErrInvalidToken) || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("discovery naming another issuer must be refused: %v", err)
	}
}

func TestVerifyProviderDownKeepsKnownKeys(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, func(c *OIDCConfig) { c.KeysRefresh = time.Minute })
	if _, err := v.Verify(context.Background(), p.token(t, nil)); err != nil {
		t.Fatal(err)
	}
	p.srv.Close()
	v.now = func() time.Time { return testNow.Add(2 * time.Minute) } // keys are stale; the refetch fails
	if _, err := v.Verify(context.Background(), p.token(t, nil)); err != nil {
		t.Fatalf("known keys must keep working while the provider is down: %v", err)
	}
}

func TestOIDCConfigValidate(t *testing.T) {
	ok := OIDCConfig{Issuer: "https://idp.example/realms/x", Audience: "nbsandbox", IDPrefix: "kc", IDClaims: []string{"sub"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*OIDCConfig){
		"http issuer":  func(c *OIDCConfig) { c.Issuer = "http://idp.example" },
		"no audience":  func(c *OIDCConfig) { c.Audience = "" },
		"bad prefix":   func(c *OIDCConfig) { c.IDPrefix = "Entra:" },
		"no id claims": func(c *OIDCConfig) { c.IDClaims = nil },
		"email as id":  func(c *OIDCConfig) { c.IDClaims = []string{"email"} },
		"upn as id":    func(c *OIDCConfig) { c.IDClaims = []string{"tid", "upn"} },
		"huge leeway":  func(c *OIDCConfig) { c.Leeway = time.Hour },
		"http jwks":    func(c *OIDCConfig) { c.JWKSURL = "http://keys.example/jwks" },
	} {
		c := ok
		c.IDClaims = append([]string(nil), ok.IDClaims...)
		mod(&c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	loop := ok
	loop.Issuer = "http://127.0.0.1:8080/realms/x"
	if err := loop.Validate(); err != nil {
		t.Fatalf("http on loopback (local IdP, tests) must be allowed: %v", err)
	}
}

func TestMiddlewareSchemes(t *testing.T) {
	p := newIDP(t)
	users, err := ParseUsers([]byte("users:\n  - username: alice\n    password_hash: " + mustHash(t, "alice-password") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	var seen Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen, _ = From(r.Context()); w.WriteHeader(204) })
	fail := func(w http.ResponseWriter, _ *http.Request, f Failure) { w.WriteHeader(f.Status) }
	both := (&Authenticator{Users: users, OIDC: p.verifier(t, nil)}).Middleware(fail, next)
	basicOnly := (&Authenticator{Users: users}).Middleware(fail, next)

	do := func(h http.Handler, set func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/spaces", nil)
		set(r)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	rec := do(both, func(*http.Request) {})
	if rec.Code != 401 || len(rec.Header().Values("WWW-Authenticate")) != 2 {
		t.Fatalf("no credentials: %d %v (want 401 with a Bearer and a Basic challenge)", rec.Code, rec.Header())
	}
	rec = do(both, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.token(t, nil)) })
	if rec.Code != 204 || seen.ID != "test:user-1" || seen.Has(ScopeFilesWrite) {
		t.Fatalf("bearer: %d %+v", rec.Code, seen)
	}
	rec = do(both, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.token(t, map[string]any{"aud": "x"})) })
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("bad token: %d %v", rec.Code, rec.Header())
	}
	rec = do(both, func(r *http.Request) { r.SetBasicAuth("alice", "alice-password") })
	if rec.Code != 204 || seen.ID != "alice" || !seen.Has(ScopeFunctionsPublish) {
		t.Fatalf("basic: %d %+v", rec.Code, seen)
	}
	rec = do(basicOnly, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.token(t, nil)) })
	if rec.Code != 401 {
		t.Fatalf("bearer with OIDC off: %d", rec.Code)
	}
	if got := rec.Header().Values("WWW-Authenticate"); len(got) != 1 || !strings.HasPrefix(got[0], "Basic") {
		t.Fatalf("challenges with OIDC off: %v", got)
	}
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
