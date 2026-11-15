// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type clientAddrKey struct{}

// WithClientAddr records the caller's network address in ctx, for the login
// limiter.
func WithClientAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, clientAddrKey{}, addr)
}

// ClientAddr is the caller's address recorded by WithClientAddr, or "".
func ClientAddr(ctx context.Context) string {
	s, _ := ctx.Value(clientAddrKey{}).(string)
	return s
}

// ClientIP returns the address the request came from: the connection's peer,
// or, when the peer is a trusted proxy (Caddy, an ingress), the last address
// in X-Forwarded-For that isn't one. Without trusted proxies the header is
// ignored: any client could write it.
func ClientIP(ctx context.Context, remoteAddr string, h http.Header, trusted *TrustedProxies) string {
	peer := hostOf(remoteAddr)
	ip, err := netip.ParseAddr(peer)
	if err != nil || !trusted.Contains(ctx, ip) {
		return peer
	}
	var hops []string
	for _, v := range h.Values("X-Forwarded-For") {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				hops = append(hops, p)
			}
		}
	}
	// From the nearest hop back: the first one that isn't a trusted proxy is
	// the client. Hops further back were written by that client.
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hostOf(hops[i]))
		if err != nil {
			return peer // a malformed header: don't trust any of it
		}
		if !trusted.Contains(ctx, a) {
			return a.String()
		}
	}
	return peer
}

func hostOf(s string) string {
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return strings.Trim(s, "[]")
}

// ParsePrefixes parses CIDRs and plain addresses (as /32 or /128).
func ParsePrefixes(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}
