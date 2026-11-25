// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// VaultConfig is where connections' secrets live in Vault or OpenBao (KV
// version 2), and how nbsandboxd and the fetchers log in.
type VaultConfig struct {
	Address string // e.g. https://vault.example:8200
	Mount   string // the KV v2 mount (default "secret")
	// Prefix is the path under Mount where nbsandbox writes managed
	// secrets, and only nbsandbox (default "nbsandbox/connections").
	Prefix string
	// AuthMount is the Kubernetes auth method's mount (default
	// "kubernetes"); Role is nbsandboxd's role there, FetcherRole the
	// fetchers'.
	AuthMount, Role, FetcherRole string
	// Token, if set, is used instead of Kubernetes auth (testing).
	Token string
	// TokenFile is nbsandboxd's service account token (default: the
	// in-cluster one).
	TokenFile string
	// References are the path prefixes a referenced secret may be under.
	// Empty: references are refused.
	References []string
	// CACertFile, if set, is the CA that signed Vault's certificate.
	CACertFile string
}

// Vault keeps connections' secrets in Vault/OpenBao. It writes managed
// ones under its prefix and checks that references exist: it never reads a
// value. Fetchers read them themselves (connections.VaultSource).
type Vault struct {
	cfg  VaultConfig
	http *http.Client

	mu    sync.Mutex
	token string
	until time.Time
}

// NewVault checks the settings and returns the provider. It doesn't log
// in until first used.
func NewVault(cfg VaultConfig) (*Vault, error) {
	u, err := url.Parse(cfg.Address)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("vault address must be an http(s) URL, got %q", cfg.Address)
	}
	if cfg.Mount == "" {
		cfg.Mount = "secret"
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "nbsandbox/connections"
	}
	if cfg.AuthMount == "" {
		cfg.AuthMount = "kubernetes"
	}
	if cfg.TokenFile == "" {
		cfg.TokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // a path, not a credential
	}
	for _, p := range append([]string{cfg.Mount, cfg.Prefix, cfg.AuthMount}, cfg.References...) {
		if !vaultPath.MatchString(p) {
			return nil, fmt.Errorf("vault: %q is not a path (letters, digits, - _ . /)", p)
		}
	}
	if cfg.Token == "" && cfg.Role == "" {
		return nil, errors.New("vault: a role (Kubernetes auth) or a token is needed")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if cfg.CACertFile != "" {
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("vault CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("vault CA: no certificate in the file")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	cfg.Address = strings.TrimSuffix(cfg.Address, "/")
	return &Vault{cfg: cfg, http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}, nil
}

var vaultPath = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

// Name implements Provider.
func (v *Vault) Name() string { return "vault" }

// Config is the provider's settings (for the fetchers' configuration).
func (v *Vault) Config() VaultConfig { return v.cfg }

func (v *Vault) managedPath(connectionID string) string {
	return v.cfg.Prefix + "/conn-" + connectionID
}

// ParseVaultRef splits a reference, path#key, checking both.
func ParseVaultRef(ref string) (path, key string, err error) {
	path, key, ok := strings.Cut(ref, "#")
	if !ok || !vaultPath.MatchString(path) || !regexp.MustCompile(`^[A-Za-z0-9_.-]{1,253}$`).MatchString(key) || strings.Contains(path, "..") {
		return "", "", fmt.Errorf("%w: a Vault reference is path#key (under the KV mount), got %q", ErrBadReference, ref)
	}
	return path, key, nil
}

// Store implements Provider: written at <prefix>/conn-<id>, key "secret".
func (v *Vault) Store(ctx context.Context, connectionID string, value []byte) (string, error) {
	p := v.managedPath(connectionID)
	body, _ := json.Marshal(map[string]any{"data": map[string]string{"secret": string(value)}})
	if _, err := v.do(ctx, http.MethodPost, "/v1/"+v.cfg.Mount+"/data/"+p, body); err != nil {
		return "", err
	}
	return p + "#secret", nil
}

// Remove implements Provider: only nbsandbox's own secrets (under its
// prefix) are deleted, with all their versions.
func (v *Vault) Remove(ctx context.Context, ref string) error {
	p, _, err := ParseVaultRef(ref)
	if err != nil || !strings.HasPrefix(p, v.cfg.Prefix+"/") {
		return nil
	}
	_, err = v.do(ctx, http.MethodDelete, "/v1/"+v.cfg.Mount+"/metadata/"+p, nil)
	if errors.Is(err, errVaultNotFound) {
		return nil
	}
	return err
}

// Check implements Provider: a reference must be under an allowed prefix,
// and exist. Only its metadata is read, never its value.
func (v *Vault) Check(ctx context.Context, ref string) error {
	p, _, err := ParseVaultRef(ref)
	if err != nil {
		return err
	}
	if !v.Referable(p) {
		return fmt.Errorf("%w: %q is not under a path the operator allows for references", ErrBadReference, p)
	}
	_, err = v.do(ctx, http.MethodGet, "/v1/"+v.cfg.Mount+"/metadata/"+p, nil)
	if errors.Is(err, errVaultNotFound) {
		return fmt.Errorf("%w: no secret at %q", ErrBadReference, p)
	}
	return err
}

// Referable reports whether a path may be referenced: under an allowed
// prefix, never nbsandbox's own.
func (v *Vault) Referable(p string) bool {
	if strings.HasPrefix(p, v.cfg.Prefix+"/") {
		return false
	}
	for _, r := range v.cfg.References {
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}

var errVaultNotFound = errors.New("not found in vault")

// do calls Vault with nbsandboxd's token, logging in first if needed.
func (v *Vault) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	tok, err := v.login(ctx)
	if err != nil {
		return nil, err
	}
	b, status, err := v.call(ctx, method, path, tok, body)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusNotFound:
		return nil, errVaultNotFound
	case status >= 300:
		return nil, fmt.Errorf("vault %s %s: %d %s", method, path, status, vaultErrors(b))
	}
	return b, nil
}

func (v *Vault) call(ctx context.Context, method, path, token string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, v.cfg.Address+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("vault: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, err
}

// vaultErrors is Vault's error list, without anything else of the body.
func vaultErrors(b []byte) string {
	var e struct{ Errors []string }
	if json.Unmarshal(b, &e) == nil && len(e.Errors) > 0 {
		return strings.Join(e.Errors, "; ")
	}
	return ""
}

// login returns nbsandboxd's token: the configured one, or one from
// Kubernetes auth, renewed before it expires.
func (v *Vault) login(ctx context.Context) (string, error) {
	if v.cfg.Token != "" {
		return v.cfg.Token, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && time.Now().Before(v.until) {
		return v.token, nil
	}
	jwt, err := os.ReadFile(v.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("vault login: the service account token: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"role": v.cfg.Role, "jwt": strings.TrimSpace(string(jwt))})
	b, status, err := v.call(ctx, http.MethodPost, "/v1/auth/"+v.cfg.AuthMount+"/login", "", body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("vault login (role %s): %d %s", v.cfg.Role, status, vaultErrors(b))
	}
	var r struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		}
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Auth.ClientToken == "" {
		return "", errors.New("vault login: no token in the answer")
	}
	v.token = r.Auth.ClientToken
	v.until = time.Now().Add(time.Duration(r.Auth.LeaseDuration)*time.Second*3/4 + time.Second)
	return v.token, nil
}
