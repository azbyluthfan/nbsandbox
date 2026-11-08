// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// Whatever the text, a size parses to a non-negative value or is refused.
func FuzzParseSize(f *testing.F) {
	for _, s := range []string{"2GiB", "1.5GB", "1048576", "-1", "NaN GiB", "Inf MiB", "1e30GiB", "0.0000001KiB", " 3 MiB "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if v, err := ParseSize(s); err == nil && v < 0 {
			t.Fatalf("%q parsed to %d", s, v)
		}
	})
}
