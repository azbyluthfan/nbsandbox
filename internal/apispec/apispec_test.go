// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package apispec

import (
	"net/http"
	"strings"
	"testing"
)

func load(t *testing.T) *Spec {
	t.Helper()
	s, err := Load("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func hdr(ct string) http.Header { return http.Header{"Content-Type": {ct}} }

func TestValidate(t *testing.T) {
	s := load(t)
	const sp = "/v1/spaces/0b9e6c1c-6d1e-4c43-9a4b-2a7f6f1f3c11"
	notFound := `{"error":{"code":"not_found","message":"space not found"}}`
	cases := []struct {
		name, method, path string
		status             int
		ct, body           string
		wantErr            string
	}{
		{"documented error body", "GET", sp, 404, "application/json", notFound, ""},
		{"error body missing message", "GET", sp, 404, "application/json", `{"error":{"code":"not_found"}}`, "doesn't match"},
		{"unknown error code", "GET", sp, 404, "application/json", `{"error":{"code":"nope","message":"x"}}`, "doesn't match"},
		{"undocumented status", "GET", sp, 418, "application/json", `{}`, "status not documented"},
		{"wrong content type", "GET", sp, 404, "text/html", "<p>", "not documented"},
		{"path with slashes", "GET", sp + "/files/data/a/b.csv", 404, "application/json", notFound, ""},
		{"unknown path", "GET", "/v2/nothing", 200, "application/json", `{}`, "path not in the spec"},
		{"not JSON", "GET", sp, 404, "application/json", `{`, "not JSON"},
		{"no body expected", "DELETE", sp, 204, "", "", ""},
		{"liveness", "GET", "/healthz", 200, "application/json", `{"status":"ok"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.Validate(c.method, c.path, c.status, hdr(c.ct), []byte(c.body))
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestCovered(t *testing.T) {
	s := load(t)
	if !s.Covered("GET", "/v1/spaces/x/runs/y/log/stream") || s.Covered("PUT", "/v1/spaces") || s.Covered("GET", "/nope") {
		t.Fatal("Covered disagrees with the spec")
	}
}
