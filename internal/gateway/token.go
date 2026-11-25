// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package gateway is the egress gateway (docs/plan-data-access.md): the only
// way out for a run's fetcher. A run's token, signed by nbsandboxd, names
// the endpoints it may reach; the gateway checks the signature, resolves
// the name itself, refuses internal addresses, and tunnels. It keeps no
// state and asks nobody.
package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Claims are what a run's token says: the run, the host:port endpoints it
// may reach, and when the token stops working.
type Claims struct {
	Run   string   `json:"run"`
	Allow []string `json:"allow"`
	Exp   int64    `json:"exp"` // Unix seconds
}

// ErrToken is a token that is malformed, forged or expired.
var ErrToken = errors.New("invalid run token")

// Sign returns a token for c, signed with key.
func Sign(key []byte, c Claims) string {
	allow := make([]string, len(c.Allow))
	for i, a := range c.Allow {
		allow[i] = strings.ToLower(a)
	}
	c.Allow = allow
	b, _ := json.Marshal(c)
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a token's signature and expiry and returns its claims.
func Verify(key []byte, tok string, now time.Time) (Claims, error) {
	var c Claims
	p, s, ok := strings.Cut(tok, ".")
	if !ok || len(key) == 0 {
		return c, ErrToken
	}
	b, err1 := base64.RawURLEncoding.DecodeString(p)
	sig, err2 := base64.RawURLEncoding.DecodeString(s)
	if err1 != nil || err2 != nil {
		return c, ErrToken
	}
	m := hmac.New(sha256.New, key)
	m.Write(b)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return c, ErrToken
	}
	if json.Unmarshal(b, &c) != nil || c.Run == "" || now.Unix() > c.Exp {
		return Claims{}, ErrToken
	}
	return c, nil
}

// Allows reports whether the claims allow host:port (case-insensitively).
func (c Claims) Allows(hostport string) bool {
	hostport = strings.ToLower(hostport)
	for _, a := range c.Allow {
		if a == hostport {
			return true
		}
	}
	return false
}
