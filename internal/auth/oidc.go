// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// OIDCConfig configures bearer-token authentication: nbsandbox is an OAuth 2.0
// resource server that accepts JWT access tokens (RFC 9068) from one issuer.
// It works with any standard identity provider (Entra ID, Keycloak, Okta,
// Auth0, ...); only the claim names differ, and they are settings.
type OIDCConfig struct {
	// Issuer must equal the token's iss exactly, e.g.
	// https://login.microsoftonline.com/<tenant>/v2.0.
	Issuer string `yaml:"issuer"`
	// Audience must be one of the token's aud values: nbsandbox's own
	// identifier at the provider (Entra: the app ID URI or client ID). A
	// token issued for another API is refused.
	Audience string `yaml:"audience"`
	// JWKSURL is where the signing keys are; empty = from the issuer's
	// discovery document (<issuer>/.well-known/openid-configuration).
	JWKSURL string `yaml:"jwks_url"`
	// IDPrefix names this provider in caller IDs, e.g. "entra" gives
	// "entra:<tid>:<oid>". Space owners are stored with it, so it must never
	// change.
	IDPrefix string `yaml:"id_prefix"`
	// IDClaims are the claims that identify the caller; they must be stable
	// and never reused (Entra: [tid, oid]; most others: [sub]). Never email
	// or upn: those change and can be reassigned.
	IDClaims []string `yaml:"id_claims"`
	// Leeway allows for clock differences when checking exp, nbf and iat
	// (default 60s, at most 5m).
	Leeway time.Duration `yaml:"leeway"`
	// MaxLifetime refuses tokens valid for longer than this (default 24h):
	// access tokens should be short-lived.
	MaxLifetime time.Duration `yaml:"max_lifetime"`
	// KeysRefresh is how often the signing keys are fetched again, and the
	// shortest gap between fetches for an unknown key ID (default 1h; the
	// gap is at most 1m).
	KeysRefresh time.Duration `yaml:"keys_refresh"`
}

var idPrefixRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

// Validate reports every problem at once.
func (c *OIDCConfig) Validate() error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if err := checkURL(c.Issuer); err != nil {
		add("auth.oidc.issuer: %v", err)
	}
	if c.Audience == "" {
		add("auth.oidc.audience: must be set (nbsandbox's identifier at the identity provider)")
	}
	if c.JWKSURL != "" {
		if err := checkURL(c.JWKSURL); err != nil {
			add("auth.oidc.jwks_url: %v", err)
		}
	}
	if !idPrefixRE.MatchString(c.IDPrefix) {
		add("auth.oidc.id_prefix: must be 1-20 lowercase letters, digits or -, starting with a letter (e.g. entra), got %q", c.IDPrefix)
	}
	if len(c.IDClaims) == 0 {
		add("auth.oidc.id_claims: must list at least one claim (e.g. [sub], or [tid, oid] for Entra ID)")
	}
	for _, cl := range c.IDClaims {
		switch strings.ToLower(cl) {
		case "", "email", "upn", "preferred_username", "unique_name", "name":
			add("auth.oidc.id_claims: %q can change or be reused; use a stable ID such as sub or oid", cl)
		}
	}
	if c.Leeway < 0 || c.Leeway > 5*time.Minute {
		add("auth.oidc.leeway: must be between 0 and 5m, got %s", c.Leeway)
	}
	if c.MaxLifetime < 0 {
		add("auth.oidc.max_lifetime: must not be negative")
	}
	return errors.Join(errs...)
}

// checkURL requires https, except for loopback addresses (tests, local IdPs).
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("must be an absolute URL, got %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return fmt.Errorf("must use https (http only for loopback addresses), got %q", raw)
}

// Signature algorithms accepted. Never "none" or HMAC: an HMAC key would be
// the public key, and anyone could sign with it.
var allowedAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512, jose.ES256, jose.ES384}

const maxTokenBytes = 16 << 10

// ErrInvalidToken is returned for any token that fails a check; the reason
// is in the wrapped message (for logs, not for the caller).
var ErrInvalidToken = errors.New("invalid token")

// Verifier checks bearer tokens against one issuer.
type Verifier struct {
	cfg    OIDCConfig
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	jwksURL   string
	keys      map[string]jose.JSONWebKey // by kid ("" = a key without one)
	fetchedAt time.Time
	lastTry   time.Time
}

// NewVerifier checks the settings and returns a verifier. Keys are fetched
// on first use, so nbsandboxd starts even while the provider is unreachable;
// tokens are refused until the keys load.
func NewVerifier(cfg OIDCConfig, client *http.Client) (*Verifier, error) {
	if cfg.Leeway == 0 {
		cfg.Leeway = 60 * time.Second
	}
	if cfg.MaxLifetime == 0 {
		cfg.MaxLifetime = 24 * time.Hour
	}
	if cfg.KeysRefresh == 0 {
		cfg.KeysRefresh = time.Hour
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Verifier{cfg: cfg, client: client, now: time.Now, jwksURL: cfg.JWKSURL}, nil
}

// Verify checks a token and returns the caller.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	bad := func(f string, a ...any) (Principal, error) {
		return Principal{}, fmt.Errorf("%w: %s", ErrInvalidToken, fmt.Sprintf(f, a...))
	}
	if len(raw) > maxTokenBytes {
		return bad("longer than %d bytes", maxTokenBytes)
	}
	jws, err := jose.ParseSignedCompact(raw, allowedAlgs)
	if err != nil {
		return bad("not a signed JWT with an accepted algorithm: %v", err)
	}
	if len(jws.Signatures) != 1 {
		return bad("must have exactly one signature")
	}
	hdr := jws.Signatures[0].Header
	// The key comes only from the issuer's key set, never from the token
	// (an embedded "jwk" header is ignored).
	key, err := v.key(ctx, hdr.KeyID)
	if err != nil {
		return bad("%v", err)
	}
	if key.Algorithm != "" && key.Algorithm != hdr.Algorithm {
		return bad("algorithm %s doesn't match the key's %s", hdr.Algorithm, key.Algorithm)
	}
	payload, err := jws.Verify(key.Key)
	if err != nil {
		return bad("signature does not verify")
	}
	var c map[string]any
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return bad("claims are not a JSON object")
	}
	if s, _ := c["iss"].(string); s != v.cfg.Issuer {
		return bad("issuer %q is not %q", s, v.cfg.Issuer)
	}
	if !hasAudience(c["aud"], v.cfg.Audience) {
		return bad("audience is not %q (a token issued for another API)", v.cfg.Audience)
	}
	now := v.now()
	exp, ok := numericDate(c["exp"])
	if !ok {
		return bad("no exp")
	}
	if now.After(exp.Add(v.cfg.Leeway)) {
		return bad("expired at %s", exp.UTC().Format(time.RFC3339))
	}
	if nbf, ok := numericDate(c["nbf"]); ok && now.Before(nbf.Add(-v.cfg.Leeway)) {
		return bad("not valid before %s", nbf.UTC().Format(time.RFC3339))
	}
	start := now
	if iat, ok := numericDate(c["iat"]); ok {
		if now.Before(iat.Add(-v.cfg.Leeway)) {
			return bad("issued in the future")
		}
		start = iat
	}
	if exp.Sub(start) > v.cfg.MaxLifetime {
		return bad("valid for %s, longer than the %s allowed", exp.Sub(start).Round(time.Second), v.cfg.MaxLifetime)
	}

	parts := make([]string, 0, len(v.cfg.IDClaims))
	for _, name := range v.cfg.IDClaims {
		s, _ := c[name].(string)
		if s == "" || len(s) > 255 || strings.ContainsAny(s, ":\x00\r\n") {
			return bad("claim %s is missing or not a usable ID", name)
		}
		parts = append(parts, s)
	}
	id := v.cfg.IDPrefix + ":" + strings.Join(parts, ":")
	return newTokenPrincipal(id, firstString(c, "azp", "appid", "client_id"), firstString(c, "name", "preferred_username"), scopesOf(c)), nil
}

// Scopes: "scp" (Entra, delegated: space-separated), "scope" (RFC 9068:
// space-separated), "roles" (Entra app roles: a list).
func scopesOf(c map[string]any) []string {
	var out []string
	for _, name := range []string{"scp", "scope"} {
		switch v := c[name].(type) {
		case string:
			out = append(out, strings.Fields(v)...)
		case []any:
			out = append(out, strings2(v)...)
		}
	}
	if v, ok := c["roles"].([]any); ok {
		out = append(out, strings2(v)...)
	}
	return out
}

func strings2(v []any) []string {
	out := make([]string, 0, len(v))
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func firstString(c map[string]any, names ...string) string {
	for _, n := range names {
		if s, ok := c[n].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func hasAudience(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func numericDate(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f <= 0 || f > 1e11 {
		return time.Time{}, false
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), true
}

// key returns the signing key with the given ID, fetching the key set when
// it's stale or the ID is unknown (no more than once a minute for unknown
// IDs, so tokens with made-up key IDs can't hammer the provider).
func (v *Verifier) key(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	k, found := v.lookup(kid)
	stale := now.Sub(v.fetchedAt) > v.cfg.KeysRefresh
	gap := min(time.Minute, v.cfg.KeysRefresh)
	if (stale || !found) && now.Sub(v.lastTry) >= gap {
		v.lastTry = now
		if err := v.fetch(ctx); err != nil {
			if !found {
				return jose.JSONWebKey{}, fmt.Errorf("signing keys unavailable: %w", err)
			}
			// Keep using the keys we have; try again after the gap.
		} else {
			k, found = v.lookup(kid)
		}
	}
	if !found {
		return jose.JSONWebKey{}, fmt.Errorf("unknown signing key %q", kid)
	}
	return k, nil
}

func (v *Verifier) lookup(kid string) (jose.JSONWebKey, bool) {
	if k, ok := v.keys[kid]; ok {
		return k, true
	}
	// A token without kid works only when the set has a single key.
	if kid == "" && len(v.keys) == 1 {
		for _, k := range v.keys {
			return k, true
		}
	}
	return jose.JSONWebKey{}, false
}

func (v *Verifier) fetch(ctx context.Context) error {
	if v.jwksURL == "" {
		var doc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, strings.TrimSuffix(v.cfg.Issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
			return fmt.Errorf("discovery: %w", err)
		}
		// OpenID Connect Discovery requires the document to name the same
		// issuer it was fetched for.
		if doc.Issuer != v.cfg.Issuer {
			return fmt.Errorf("discovery: the document's issuer %q is not %q", doc.Issuer, v.cfg.Issuer)
		}
		if err := checkURL(doc.JWKSURI); err != nil {
			return fmt.Errorf("discovery: jwks_uri %w", err)
		}
		v.jwksURL = doc.JWKSURI
	}
	var set jose.JSONWebKeySet
	if err := v.getJSON(ctx, v.jwksURL, &set); err != nil {
		return fmt.Errorf("key set: %w", err)
	}
	keys := map[string]jose.JSONWebKey{}
	for _, k := range set.Keys {
		if (k.Use != "" && k.Use != "sig") || !k.Valid() || !k.IsPublic() {
			continue
		}
		keys[k.KeyID] = k
	}
	if len(keys) == 0 {
		return errors.New("key set has no public signing keys")
	}
	v.keys, v.fetchedAt = keys, v.now()
	return nil
}

func (v *Verifier) getJSON(ctx context.Context, u string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
