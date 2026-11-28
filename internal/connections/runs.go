// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/gateway"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/secrets"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Resolve settles which connection each of a space's names means for a
// run: in environment, or the space's default if "". A space that binds
// nothing resolves to no environment and no names.
func (s *Service) Resolve(ctx context.Context, spaceID, environment string) (string, map[string]string, error) {
	sc, err := s.d.DB.GetSpaceConnections(ctx, spaceID)
	if err != nil {
		return "", nil, err
	}
	if len(sc.Environments) == 0 {
		if environment != "" {
			return "", nil, &spaces.InvalidError{Msg: fmt.Sprintf("environment %q: this space has no environments", environment)}
		}
		return "", nil, nil
	}
	env := environment
	if env == "" {
		env = sc.Default
	}
	names, ok := sc.Environments[env]
	if !ok {
		envs := make([]string, 0, len(sc.Environments))
		for e := range sc.Environments {
			envs = append(envs, e)
		}
		sort.Strings(envs)
		return "", nil, &spaces.InvalidError{Msg: fmt.Sprintf("environment %q: this space's environments are %s", env, strings.Join(envs, ", "))}
	}
	out := make(map[string]string, len(names))
	for n, id := range names {
		out[n] = id
	}
	return env, out, nil
}

// Types returns each name's connection type (the owner's connections).
func (s *Service) Types(ctx context.Context, owner string, names map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for name, id := range names {
		c, err := s.d.DB.ConnectionByID(ctx, id)
		if err != nil || c.Owner != owner {
			return nil, fmt.Errorf("%w: %q's connection no longer exists", ErrUnavailable, name)
		}
		out[name] = c.Type
	}
	return out, nil
}

// Revealer is a secret provider that can hand a value to nbsandboxd, which
// passes it to a run's fetcher (the built-in store, on Docker).
type Revealer interface {
	Reveal(ctx context.Context, ref string) ([]byte, error)
}

// Mounter is a secret provider whose values the platform mounts into the
// fetcher itself (Kubernetes Secrets): nbsandboxd names them, never reads
// them.
type Mounter interface {
	Mount(ref string) (secret, key string, err error)
}

// FetcherSettings are the operator's settings a fetcher is made with
// (Deps.Fetcher).
type FetcherSettings struct {
	Image            string
	GatewayAddress   string // host:port the fetcher reaches the gateway at
	GatewayContainer string // Docker: the gateway's container, joined to the run's network
	GatewayKey       []byte
	MemoryBytes      int64 // each fetcher's limits
	NanoCPUs         int64
	// ServiceAccount is the fetchers' service account (Vault's Kubernetes
	// auth binds its role to it); FetcherRole that role.
	ServiceAccount, FetcherRole string
}

// ErrUnavailable is a run whose connections can't be given to it now: a
// connection deleted since it was queued, or one the operator no longer
// allows. The message says which.
var ErrUnavailable = errors.New("a connection is unavailable")

type fetcherSource struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Settings map[string]any `json:"settings"`
	Secret   string         `json:"secret,omitempty"`
	// SecretFile: the credential is mounted there instead (Kubernetes).
	SecretFile string `json:"secret_file,omitempty"`
	// SecretVault: the fetcher reads it from Vault itself.
	SecretVault *vaultSecret `json:"secret_vault,omitempty"`
	// Listen: the loopback address of the source's forwarder, which its host
	// name resolves to in the fetcher (MySQL with tls verify: its client
	// checks the name it connected to, and skips the check for 127.0.0.1).
	Listen string `json:"listen,omitempty"`
}

type vaultSecret struct {
	Path string `json:"path"`
	Key  string `json:"key"`
}

// fetcherVault is how a fetcher logs in to Vault and where it reads.
type fetcherVault struct {
	Address   string `json:"address"`
	Mount     string `json:"mount"`
	AuthMount string `json:"auth_mount"`
	Role      string `json:"role"`
	TokenFile string `json:"token_file"`
	CAPEM     string `json:"ca_pem,omitempty"`
}

type fetcherConfig struct {
	Token   string `json:"token"`
	Gateway struct {
		Address string `json:"address"`
		Token   string `json:"token"`
	} `json:"gateway"`
	Sources []fetcherSource `json:"sources"`
	Vault   *fetcherVault   `json:"vault,omitempty"`
}

// served are the types a fetcher serves: in SQL, or HTTP APIs through it.
// Azure Blob is not read yet.
var served = map[string]bool{TypePostgreSQL: true, TypeMySQL: true, TypeSQLServer: true, TypeS3: true, TypeHTTP: true}

// Fetcher makes a run's fetcher: its configuration with the credentials of
// each name the run resolved, and a gateway token for exactly their
// endpoints, valid until expires. Nil if the run reads nothing.
func (s *Service) Fetcher(ctx context.Context, runID, owner string, names map[string]string, expires time.Time) (*isolation.Fetcher, error) {
	fs := s.d.Fetcher
	var cfg fetcherConfig
	var allow []string
	var mounts []isolation.SecretMount
	hosts := map[string]string{}
	keys := make([]string, 0, len(names))
	for n := range names {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, name := range keys {
		c, err := s.d.DB.ConnectionByID(ctx, names[name])
		if err != nil || c.Owner != owner {
			return nil, fmt.Errorf("%w: %q's connection no longer exists", ErrUnavailable, name)
		}
		if !served[c.Type] {
			continue
		}
		var settings map[string]any
		if err := json.Unmarshal([]byte(c.Settings), &settings); err != nil {
			return nil, err
		}
		host, err := endpoint(c.Type, settings)
		if err != nil {
			return nil, err
		}
		for _, h := range []string{host, tokenHost(c.Type, settings)} {
			if h != "" && !s.d.Allowed.Permits(h) {
				return nil, fmt.Errorf("%w: %q's connection points to %s, which the operator no longer allows", ErrUnavailable, name, h)
			}
		}
		src := fetcherSource{Name: name, Type: c.Type, Settings: settings}
		if c.SecretMode != store.SecretNone {
			if c.SecretProvider != s.d.Secrets.Name() {
				return nil, fmt.Errorf("%w: %q's credentials are kept by the %s provider, which this nbsandbox no longer uses",
					ErrUnavailable, name, c.SecretProvider)
			}
			switch p := s.d.Secrets.(type) {
			case *secrets.Vault:
				path, key, err := secrets.ParseVaultRef(c.SecretRef)
				if err != nil {
					return nil, fmt.Errorf("%w: %q's credentials: %w", ErrUnavailable, name, err)
				}
				src.SecretVault = &vaultSecret{Path: path, Key: key}
				if cfg.Vault == nil {
					if cfg.Vault, err = fetcherVaultFor(p.Config(), fs); err != nil {
						return nil, err
					}
					allow = append(allow, hostPort(cfg.Vault.Address))
				}
			case Revealer:
				v, err := p.Reveal(ctx, c.SecretRef)
				if err != nil {
					return nil, fmt.Errorf("%w: %q's credentials: %w", ErrUnavailable, name, err)
				}
				src.Secret = string(v)
			case Mounter:
				secret, key, err := p.Mount(c.SecretRef)
				if err != nil {
					return nil, fmt.Errorf("%w: %q's credentials: %w", ErrUnavailable, name, err)
				}
				file := strconv.Itoa(len(mounts))
				mounts = append(mounts, isolation.SecretMount{Secret: secret, Key: key, Path: file})
				src.SecretFile = isolation.FetcherSecretsDir + "/" + file
			default:
				return nil, fmt.Errorf("%w: the %s provider can't give a run its credentials", ErrUnavailable, c.SecretProvider)
			}
		}
		if c.Type == TypeMySQL && str(settings, "tls") == TLSVerify {
			h := strings.ToLower(str(settings, "host"))
			if hosts[h] == "" {
				hosts[h] = fmt.Sprintf("127.0.0.%d", len(hosts)+2)
			}
			src.Listen = hosts[h]
		}
		cfg.Sources = append(cfg.Sources, src)
		allow = append(allow, targets(c.Type, settings)...)
	}
	if len(cfg.Sources) == 0 {
		return nil, nil
	}
	cfg.Token = randomToken()
	cfg.Gateway.Address = fs.GatewayAddress
	cfg.Gateway.Token = gateway.Sign(fs.GatewayKey, gateway.Claims{Run: runID, Allow: allow, Exp: expires.Unix()})
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return &isolation.Fetcher{Image: fs.Image, Config: b, Token: cfg.Token, Gateway: fs.GatewayContainer, Mounts: mounts, Hosts: hosts,
		MemoryBytes: fs.MemoryBytes, NanoCPUs: fs.NanoCPUs, VaultLogin: cfg.Vault != nil, ServiceAccount: fs.ServiceAccount}, nil
}

// targets are the host:port endpoints a source's fetcher connects to,
// through the gateway.
func targets(typ string, s map[string]any) []string {
	switch typ {
	case TypePostgreSQL, TypeMySQL, TypeSQLServer:
		return []string{net.JoinHostPort(str(s, "host"), strconv.Itoa(port(s)))}
	case TypeS3:
		host, p, err := net.SplitHostPort(str(s, "endpoint"))
		if err != nil {
			host, p = str(s, "endpoint"), "80"
			if ssl, _ := s["use_ssl"].(bool); ssl {
				p = "443"
			}
		}
		if str(s, "url_style") == "vhost" {
			host = str(s, "bucket") + "." + host
		}
		return []string{net.JoinHostPort(host, p)}
	case TypeHTTP:
		out := []string{hostPort(str(s, "url"))}
		if tokenHost(typ, s) != "" {
			out = append(out, hostPort(str(s, "token_url")))
		}
		return out
	}
	return nil
}

// port is an int setting, from the store's JSON (float64) or Go (int).
func port(s map[string]any) int {
	switch v := s["port"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fetcherVaultFor is how fetchers log in to Vault: the fetchers' role, a
// token of their own for Vault's audience, and Vault's CA if it has one.
func fetcherVaultFor(c secrets.VaultConfig, fs FetcherSettings) (*fetcherVault, error) {
	v := &fetcherVault{Address: c.Address, Mount: c.Mount, AuthMount: c.AuthMount, Role: fs.FetcherRole,
		TokenFile: isolation.FetcherVaultTokenPath}
	if c.CACertFile != "" {
		pem, err := os.ReadFile(c.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("vault CA: %w", err)
		}
		v.CAPEM = string(pem)
	}
	return v, nil
}

// hostPort is a URL's host:port, with the scheme's port if it has none.
func hostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Port() != "" {
		return strings.ToLower(u.Host)
	}
	p := "443"
	if u.Scheme == "http" {
		p = "80"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), p)
}
