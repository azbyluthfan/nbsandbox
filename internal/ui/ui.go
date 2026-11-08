// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package ui serves the optional local web UI's static files: plain HTML, CSS and JavaScript embedded in the binary, with no
// build step and no external resources.
package ui

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFS embed.FS

// ContentSecurityPolicy for the UI's files: scripts and styles from this
// origin only, notebook images as data: URLs, no frames, nothing else. The
// rendered notebook opens in its own tab under its own sandbox CSP.
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; frame-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// pageCSP is the policy for the page itself: as above, plus styles that
// carry this response's nonce. The editor (CodeMirror) adds its base styles
// as a <style> element; it reads the nonce from the page's
// <meta name="csp-nonce"> and tags the element with it. No 'unsafe-inline'.
func pageCSP(nonce string) string {
	return strings.Replace(ContentSecurityPolicy, "style-src 'self'", "style-src 'self' 'nonce-"+nonce+"'", 1)
}

const nonceMeta = `<meta name="csp-nonce" content="">`

// Static returns the handler for the UI's files.
func Static() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil || !strings.Contains(string(index), nonceMeta) {
		panic("web/index.html must contain " + nonceMeta)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			nonce := newNonce()
			h.Set("Content-Security-Policy", pageCSP(nonce))
			h.Set("Content-Type", "text/html; charset=utf-8")
			h.Set("Cache-Control", "no-store") // a fresh nonce on every load
			page := strings.Replace(string(index), nonceMeta, `<meta name="csp-nonce" content="`+nonce+`">`, 1)
			_, _ = io.WriteString(w, page)
			return
		}
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		files.ServeHTTP(w, r)
	})
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
