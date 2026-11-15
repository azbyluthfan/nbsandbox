// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// TrustedProxies are the proxies whose X-Forwarded-For is believed: CIDRs,
// addresses, and host names (Compose service names such as "caddy"), which
// are looked up again every Refresh, since a container's address can change.
type TrustedProxies struct {
	prefixes []netip.Prefix
	hosts    []string
	lookup   func(ctx context.Context, host string) ([]netip.Addr, error)
	refresh  time.Duration

	mu       sync.Mutex
	resolved []netip.Addr
	at       time.Time
}

// NewTrustedProxies parses the list; anything that isn't an address or a
// CIDR is a host name. An empty list trusts no one (nil).
func NewTrustedProxies(list []string) (*TrustedProxies, error) {
	t := &TrustedProxies{refresh: 30 * time.Second, lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}}
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := ParsePrefixes([]string{s}); err == nil {
			t.prefixes = append(t.prefixes, p...)
			continue
		}
		if !validHost(s) {
			return nil, &net.ParseError{Type: "proxy address, CIDR or host name", Text: s}
		}
		t.hosts = append(t.hosts, s)
	}
	if len(t.prefixes) == 0 && len(t.hosts) == 0 {
		return nil, nil
	}
	return t, nil
}

func validHost(s string) bool {
	if len(s) > 253 || strings.Trim(s, ".-") == "" {
		return false
	}
	for _, r := range s {
		if r != '.' && r != '-' && r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// Contains reports whether ip is a trusted proxy.
func (t *TrustedProxies) Contains(ctx context.Context, ip netip.Addr) bool {
	if t == nil {
		return false
	}
	ip = ip.Unmap()
	for _, p := range t.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	if len(t.hosts) == 0 {
		return false
	}
	for _, a := range t.addrs(ctx) {
		if a.Unmap() == ip {
			return true
		}
	}
	return false
}

// addrs are the host names' addresses, looked up at most every refresh. A
// failed lookup keeps the last known addresses. The lookup outlives a
// cancelled request: its answer serves the requests after it.
func (t *TrustedProxies) addrs(ctx context.Context) []netip.Addr {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.at.IsZero() && time.Since(t.at) < t.refresh {
		return t.resolved
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var out []netip.Addr
	ok := false
	for _, h := range t.hosts {
		as, err := t.lookup(ctx, h)
		if err != nil {
			continue
		}
		ok = true
		out = append(out, as...)
	}
	if ok || t.at.IsZero() {
		t.resolved = out
	}
	t.at = time.Now()
	return t.resolved
}
