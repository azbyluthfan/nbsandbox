// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package deps

import (
	"regexp"
	"testing"
)

var moduleName = regexp.MustCompile(`^[A-Za-z_]\w*$`)

// The scanners never panic and only report plausible names.
func FuzzScanners(f *testing.F) {
	for _, s := range []string{"import os, sys as s\nfrom a.b import c", "%pip install x", "pandas>=2 ; x\n-r y", "\x00\xff", "import"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		for _, m := range importsIn(src) {
			if !moduleName.MatchString(m) {
				t.Fatalf("importsIn(%q) gave %q", src, m)
			}
		}
		for _, n := range requirementNames([]byte(src)) {
			if n == "" || normalize(n) != n {
				t.Fatalf("requirementNames(%q) gave %q", src, n)
			}
		}
	})
}
