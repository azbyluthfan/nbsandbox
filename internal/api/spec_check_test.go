// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/apispec"
)

var (
	specOnce sync.Once
	spec     *apispec.Spec
	specErr  error
)

// checkSpec wraps h so every response a test receives is checked against
// api/openapi.yaml; mismatches fail the test when it ends. For the web UI
// handler (uiUser non-nil), /ui/api/as/{user}/v1/... is checked as /v1/...
// for known users; the UI's own answers (unknown user, paths outside /v1)
// aren't part of the API spec.
//
// Unclean paths (with "..", "//") are skipped: Go's router redirects them
// before any handler runs, and real clients don't send them.
func checkSpec(t *testing.T, h http.Handler, uiUser func(string) bool) http.Handler {
	t.Helper()
	specOnce.Do(func() { spec, specErr = apispec.Load("../../api/openapi.yaml") })
	if specErr != nil {
		t.Fatal(specErr)
	}
	var mu sync.Mutex
	var problems []string
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range problems {
			t.Errorf("response doesn't match api/openapi.yaml: %s", p)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if uiUser != nil {
			// /ui/api/as/{user}/v1/... → /v1/...
			rest, _ := strings.CutPrefix(p, "/ui/api/as/")
			user, sub, _ := strings.Cut(rest, "/")
			p = "/" + sub
			if !strings.HasPrefix(p, "/v1/") || !uiUser(user) {
				h.ServeHTTP(w, r)
				return
			}
		}
		if path.Clean(p) != p || !spec.Covered(r.Method, p) {
			h.ServeHTTP(w, r)
			return
		}
		rec := &specRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		body := rec.body.Bytes()
		if w.Header().Get("Content-Encoding") == "gzip" {
			if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				body, _ = io.ReadAll(zr)
			}
		}
		if err := spec.Validate(r.Method, p, rec.status, w.Header(), body); err != nil {
			mu.Lock()
			problems = append(problems, err.Error())
			mu.Unlock()
		}
	})
}

// specRecorder passes everything through and keeps a copy of the status and
// body. Streams (SSE) aren't copied: only their content type is checked.
type specRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *specRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *specRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if !strings.HasPrefix(r.Header().Get("Content-Type"), "text/event-stream") && r.body.Len() < 8<<20 {
		r.body.Write(b)
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and deadlines.
func (r *specRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush keeps handlers that type-assert http.Flusher working.
func (r *specRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
