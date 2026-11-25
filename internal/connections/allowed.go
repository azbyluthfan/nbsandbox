// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"net/netip"
	"strings"
)

// Allowed is the operator's list of where connections may point
// (connections.allowed): host names, "*.domain" patterns and CIDR ranges.
type Allowed struct {
	names    map[string]bool
	suffixes []string // ".domain" for "*.domain"
	prefixes []netip.Prefix
}

// NewAllowed parses the entries (already validated by config).
func NewAllowed(entries []string) Allowed {
	a := Allowed{names: map[string]bool{}}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case strings.Contains(e, "/"):
			if p, err := netip.ParsePrefix(e); err == nil {
				a.prefixes = append(a.prefixes, p.Masked())
			}
		case strings.HasPrefix(e, "*."):
			a.suffixes = append(a.suffixes, e[1:])
		case e != "":
			a.names[e] = true
		}
	}
	return a
}

// Permits reports whether a connection may point to host. Loopback,
// link-local (where cloud metadata lives), unspecified and multicast
// addresses are refused whatever the list says, as is "localhost". A host
// name matches a name or a pattern; an address, a CIDR range. Names are
// resolved later, by the egress gateway, which checks the addresses too.
func (a Allowed) Permits(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return false
		}
		for _, p := range a.prefixes {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	if a.names[host] {
		return true
	}
	for _, s := range a.suffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}
