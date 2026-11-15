// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// IntrospectionConfig turns on token introspection (RFC 7662): after a token
// passes the local checks, the provider is asked whether it is still
// active, so a token revoked at the provider (the user signed out, was
// disabled, the session was ended) stops working within Cache, not only
// when it expires. Keycloak, Okta and Auth0 support it; Entra ID doesn't.
type IntrospectionConfig struct {
	// URL is the introspection endpoint; empty = the provider's discovery
	// document's introspection_endpoint.
	URL string `yaml:"url"`
	// ClientID and ClientSecret are nbsandbox's own client at the provider,
	// allowed to introspect. Prefer NBSANDBOX_OIDC_INTROSPECTION_CLIENT_SECRET
	// for the secret; it is never logged.
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// Cache is how long an answer is reused (default 30s, at most 5m): the
	// longest a revoked token can still be used.
	Cache time.Duration `yaml:"cache"`
}

func (c *IntrospectionConfig) validate(add func(string, ...any)) {
	if c.URL != "" {
		if err := checkURL(c.URL); err != nil {
			add("auth.oidc.introspection.url: %v", err)
		}
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		add("auth.oidc.introspection: client_id and client_secret (or NBSANDBOX_OIDC_INTROSPECTION_CLIENT_SECRET) must be set")
	}
	if c.Cache < 0 || c.Cache > 5*time.Minute {
		add("auth.oidc.introspection.cache: must be between 0 and 5m, got %s", c.Cache)
	}
}

// introspector asks the provider about tokens and remembers the answers.
type introspector struct {
	cfg    IntrospectionConfig
	issuer string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	url   string
	cache map[[32]byte]introAnswer
}

type introAnswer struct {
	active bool
	until  time.Time
}

const maxIntroCache = 50_000

// active reports whether the provider still considers the token active.
// exp is the token's own expiry: no answer is kept past it.
func (in *introspector) active(ctx context.Context, raw string, exp time.Time) error {
	k := sha256.Sum256([]byte(raw))
	now := in.now()
	in.mu.Lock()
	a, ok := in.cache[k]
	in.mu.Unlock()
	if ok && now.Before(a.until) {
		if !a.active {
			return errors.New("revoked (introspection says inactive)")
		}
		return nil
	}
	active, err := in.ask(ctx, raw)
	if err != nil {
		// Fail closed: a token can't be shown to be still valid.
		return fmt.Errorf("introspection failed: %w", err)
	}
	until := now.Add(in.cfg.Cache)
	if !active || exp.Before(until) {
		until = exp // an inactive token never becomes active again
	}
	in.mu.Lock()
	if len(in.cache) >= maxIntroCache {
		for key, v := range in.cache {
			if !now.Before(v.until) {
				delete(in.cache, key)
			}
		}
		if len(in.cache) >= maxIntroCache {
			clear(in.cache)
		}
	}
	in.cache[k] = introAnswer{active: active, until: until}
	in.mu.Unlock()
	if !active {
		return errors.New("revoked (introspection says inactive)")
	}
	return nil
}

func (in *introspector) ask(ctx context.Context, raw string) (bool, error) {
	endpoint, err := in.endpoint(ctx)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	form := url.Values{"token": {raw}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(in.cfg.ClientID), url.QueryEscape(in.cfg.ClientSecret))
	resp, err := in.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return false, err
	}
	return out.Active, nil
}

// endpoint is the configured URL, or the one the discovery document names.
func (in *introspector) endpoint(ctx context.Context) (string, error) {
	in.mu.Lock()
	u := in.url
	in.mu.Unlock()
	if u != "" {
		return u, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(in.issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := in.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Issuer   string `json:"issuer"`
		Endpoint string `json:"introspection_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	if doc.Issuer != in.issuer {
		return "", fmt.Errorf("discovery: the document's issuer %q is not %q", doc.Issuer, in.issuer)
	}
	if doc.Endpoint == "" {
		return "", errors.New("discovery: the provider has no introspection_endpoint (set auth.oidc.introspection.url)")
	}
	if err := checkURL(doc.Endpoint); err != nil {
		return "", fmt.Errorf("discovery: introspection_endpoint %w", err)
	}
	in.mu.Lock()
	in.url = doc.Endpoint
	in.mu.Unlock()
	return doc.Endpoint, nil
}
