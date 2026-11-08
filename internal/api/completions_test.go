// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/completions"
)

func TestCompletions(t *testing.T) {
	e := newEnv(t)
	index := `{"schema_version":1,"modules":{"pandas":[["read_csv","f","(path)","Read a CSV file."]]}}`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(index))
	_ = zw.Close()
	sum := sha256.Sum256(gz.Bytes())
	ref := &completions.Ref{SHA256: hex.EncodeToString(sum[:]), Bytes: int64(gz.Len())}

	d := e.deps
	base := d.Runtime
	d.Runtime = func() (RuntimeInfo, error) {
		ri, err := base()
		ri.Completions = ref
		return ri, err
	}
	d.Completions = func(_ context.Context, r completions.Ref) (io.ReadCloser, error) {
		if r != *ref {
			return nil, completions.ErrNone
		}
		return io.NopCloser(bytes.NewReader(gz.Bytes())), nil
	}
	h := checkSpec(t, NewHandler(d), nil)
	get := func(headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/runtime/completions", nil)
		r.SetBasicAuth("alice", "alice-password")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	rec := get(map[string]string{"Accept-Encoding": "gzip, br"})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(rec.Body.Bytes(), gz.Bytes()) ||
		rec.Header().Get("ETag") != `"`+ref.SHA256+`"` {
		t.Fatalf("gzip: %d %v", rec.Code, rec.Header())
	}
	if rec := get(nil); rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != index {
		t.Fatalf("plain: %d %q", rec.Code, rec.Body)
	}
	if rec := get(map[string]string{"Accept-Encoding": "gzip;q=0"}); rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("gzip sent though refused")
	}
	if rec := get(map[string]string{"If-None-Match": `"` + ref.SHA256 + `"`}); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	// The image has none: 404.
	if rec := e.req("GET", "/v1/runtime/completions", "alice", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("no index: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/runtime/completions", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
}
