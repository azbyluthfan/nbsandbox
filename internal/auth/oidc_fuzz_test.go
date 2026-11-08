// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"strings"
	"testing"
)

// Whatever arrives as a bearer token, checking it never panics and never
// accepts it: only the provider's key can sign a valid one. Seeds include a
// genuinely valid token and pieces of one.
func FuzzVerify(f *testing.F) {
	p := newIDP(f)
	v := p.verifier(f, nil)
	valid := p.token(f, nil)
	parts := strings.Split(valid, ".")
	for _, seed := range []string{"", ".", "..", "a.b.c", parts[0] + "." + parts[1] + ".", parts[0] + ".." + parts[2],
		parts[1] + "." + parts[0] + "." + parts[2], valid + "x", strings.ToUpper(valid)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, tok string) {
		if tok == valid {
			return
		}
		if p, err := v.Verify(context.Background(), tok); err == nil {
			t.Fatalf("accepted a token the provider never signed: %q → %+v", tok, p)
		}
	})
}
