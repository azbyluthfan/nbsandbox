// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/base64"
	"testing"
)

// Basic credentials decode without panicking, and a valid encoding of
// user:password comes back exactly.
func FuzzParseBasic(f *testing.F) {
	for _, s := range []string{"alice", "pass:with:colons", "", "x"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, user, pass string) {
		_, _, _ = parseBasic(user) // arbitrary header text
		if containsColon(user) {
			return
		}
		u, p, ok := parseBasic(base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)))
		if !ok || u != user || p != pass {
			t.Fatalf("%q:%q came back as %q:%q (%v)", user, pass, u, p, ok)
		}
	})
}

func containsColon(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return true
		}
	}
	return false
}
