// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticServesEmbeddedApp(t *testing.T) {
	h := Static()
	for _, name := range []string{"index.html", "app.css", "app.js", "api.js", "dom.js", "viewers.js", "run.js"} {
		if _, err := fs.Stat(webFS, "web/"+name); err != nil {
			t.Errorf("%s not embedded", name)
		}
	}
	// The page: a fresh nonce on every load, the same in its CSP and its
	// <meta>, and nothing else loosened.
	nonces := map[string]bool{}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		csp := rec.Header().Get("Content-Security-Policy")
		_, after, ok := strings.Cut(csp, "'nonce-")
		nonce, _, _ := strings.Cut(after, "'")
		if rec.Code != http.StatusOK || !ok || len(nonce) < 20 || rec.Header().Get("Referrer-Policy") != "no-referrer" ||
			!strings.Contains(rec.Body.String(), `<meta name="csp-nonce" content="`+nonce+`">`) ||
			strings.Contains(csp, "unsafe") || csp != pageCSP(nonce) {
			t.Fatalf("index: %d %v", rec.Code, rec.Header())
		}
		nonces[nonce] = true
	}
	if len(nonces) != 2 {
		t.Fatal("the nonce must change on every load")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != ContentSecurityPolicy {
		t.Fatalf("app.js: %d %v", rec.Code, rec.Header())
	}
}

// The page loads nothing from other origins and runs no inline script or
// style, so the strict CSP holds. The vendored editor bundle is checked
// separately (TestVendoredBundle).
func TestNoExternalOrInlineCode(t *testing.T) {
	_ = fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(p, "web/vendor/") {
			return nil
		}
		b, _ := fs.ReadFile(webFS, p)
		// The SVG namespace is an XML name, never fetched: icons need it.
		s := strings.ReplaceAll(string(b), `xmlns="http://www.w3.org/2000/svg"`, "")
		for _, bad := range []string{"http://", "https://", "innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "style=\"", "onclick=\""} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		return nil
	})
}

// The vendored editor bundle is exactly what make ui-vendor produced (its
// SHA256SUMS match), and contains nothing that evaluates strings as code or
// writes HTML.
func TestVendoredBundle(t *testing.T) {
	sums, err := fs.ReadFile(webFS, "web/vendor/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		want, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("bad SHA256SUMS line %q", line)
		}
		b, err := fs.ReadFile(webFS, "web/vendor/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != want {
			t.Errorf("%s: checksum %s, SHA256SUMS says %s (rebuild with make ui-vendor)", name, got, want)
		}
		n++
	}
	if n < 2 {
		t.Fatal("SHA256SUMS lists too few files")
	}
	b, _ := fs.ReadFile(webFS, "web/vendor/codemirror.js")
	for _, bad := range []string{"eval(", "new Function", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "fetch(", "XMLHttpRequest", "WebSocket"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("the editor bundle contains %q", bad)
		}
	}
}
