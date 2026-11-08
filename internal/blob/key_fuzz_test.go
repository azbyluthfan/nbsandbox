// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"strings"
	"testing"
)

// A key that passes is a relative path of plain segments: it can't climb
// out of the store's prefix or address anything odd.
func FuzzValidKey(f *testing.F) {
	for _, s := range []string{"spaces/a/b", "../x", "a/./b", "/abs", "a//b", "A", "a\\b", "sha256/ab/cd/abcd"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		if !ValidKey(key) {
			return
		}
		if len(key) > MaxKeyLen || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.Contains(key, "//") {
			t.Fatalf("accepted %q", key)
		}
		for _, seg := range strings.Split(key, "/") {
			if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\\:\x00") {
				t.Fatalf("accepted %q", key)
			}
		}
	})
}
