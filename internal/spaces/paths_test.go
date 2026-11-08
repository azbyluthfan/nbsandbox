// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePath(t *testing.T) {
	good := []string{
		"forecast.ipynb", "data/sales.csv", "a/b/c/d.txt", "notebooks/2026 Q4.ipynb",
		"résumé.csv", "x.nbsandbox", "runs.csv", "data/runs/x.csv", ".hidden", "..data", "a..b",
	}
	for _, p := range good {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want ok", p, err)
		}
	}
	bad := map[string]string{
		"":                              "empty",
		"/etc/passwd":                   "absolute",
		"../other-space/canary.txt":     "parent",
		"data/../../x":                  "parent inside",
		"data/..":                       "trailing parent",
		"./x":                           "dot segment",
		"a//b":                          "empty segment",
		"a/":                            "trailing slash",
		`a\b`:                           "backslash",
		`..\x`:                          "windows parent",
		"a\x00b":                        "NUL",
		"a\nb":                          "newline",
		".nbsandbox-status.json":        "internal name",
		"data/.nbsandbox-params.json":   "internal name nested",
		"\xff\xfe":                      "invalid utf-8",
		strings.Repeat("a", 256):        "long segment",
		strings.Repeat("a/", 600) + "x": "long path",
		"%2e%2e/x":                      "", // literal %, fine: decoding happened before; this is a real file name
	}
	for p, why := range bad {
		err := ValidatePath(p)
		if why == "" {
			if err != nil {
				t.Errorf("ValidatePath(%q) = %v, want ok (literal percent signs)", p, err)
			}
			continue
		}
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Errorf("ValidatePath(%q) [%s] = %v, want *PathError", p, why, err)
		}
	}
}

func TestValidateDir(t *testing.T) {
	if err := ValidateDir(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDir("data"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDir("../x"); err == nil {
		t.Fatal("../x must be rejected")
	}
}

// Fuzzing: no accepted path may resolve outside the space or into
// the reserved areas, whatever bytes it contains.
func FuzzValidatePath(f *testing.F) {
	for _, s := range []string{"a.txt", "../x", "a/../../b", "runs/x", "/abs", `a\b`, "a//b", ".nbsandbox", "x/./y", "\x00", "é/ü"} {
		f.Add(s)
	}
	root := filepath.FromSlash("/srv/nbsandbox/spaces/0d0f8a8e-1111-4111-8111-111111111111")
	f.Fuzz(func(t *testing.T, p string) {
		if ValidatePath(p) != nil {
			return
		}
		joined := filepath.Join(root, filepath.FromSlash(p))
		rel, err := filepath.Rel(root, joined)
		if err != nil || !filepath.IsLocal(rel) || rel != filepath.FromSlash(p) {
			t.Fatalf("accepted %q resolves to %q (rel %q)", p, joined, rel)
		}
		if strings.Contains(p, "\\") || strings.Contains(p, "\x00") {
			t.Fatalf("accepted reserved or unsafe path %q", p)
		}
	})
}
