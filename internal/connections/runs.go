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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/gateway"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
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
}

type fetcherConfig struct {
	Token   string `json:"token"`
	Gateway struct {
		Address string `json:"address"`
		Token   string `json:"token"`
	} `json:"gateway"`
	Sources []fetcherSource `json:"sources"`
}

// served are the types a fetcher reads; the others (http) reach notebooks
// another way.
var served = map[string]bool{TypePostgreSQL: true, TypeMySQL: true, TypeSQLServer: true, TypeS3: true}

// Fetcher makes a run's fetcher: its configuration with the credentials of
// each name the run resolved, and a gateway token for exactly their
// endpoints, valid until expires. Nil if the run reads nothing.
func (s *Service) Fetcher(ctx context.Context, runID, owner string, names map[string]string, expires time.Time) (*isolation.Fetcher, error) {
	fs := s.d.Fetcher
	var cfg fetcherConfig
	var allow []string
	var mounts []isolation.SecretMount
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
		if !s.d.Allowed.Permits(host) {
			return nil, fmt.Errorf("%w: %q's connection points to %s, which the operator no longer allows", ErrUnavailable, name, host)
		}
		src := fetcherSource{Name: name, Type: c.Type, Settings: settings}
		if c.SecretMode != store.SecretNone {
			if c.SecretProvider != s.d.Secrets.Name() {
				return nil, fmt.Errorf("%w: %q's credentials are kept by the %s provider, which this nbsandbox no longer uses",
					ErrUnavailable, name, c.SecretProvider)
			}
			switch p := s.d.Secrets.(type) {
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
	return &isolation.Fetcher{Image: fs.Image, Config: b, Token: cfg.Token, Gateway: fs.GatewayContainer, Mounts: mounts,
		MemoryBytes: fs.MemoryBytes, NanoCPUs: fs.NanoCPUs}, nil
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
