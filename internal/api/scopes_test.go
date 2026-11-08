// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/auth/authtest"
)

// Every authenticated route names a known scope, and api/openapi.yaml lists
// the same one for bearerAuth.
func TestRouteScopesMatchSpec(t *testing.T) {
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"` // a path also holds "parameters"
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, rt := range Routes(newEnv(t).deps) {
		if !rt.Auth {
			continue
		}
		if !slices.Contains(auth.AllScopes, rt.Scope()) {
			t.Errorf("%s %s: scope %q is not one of auth.AllScopes", rt.Method, rt.Pattern, rt.Scope())
			continue
		}
		node, ok := doc.Paths[strings.ReplaceAll(rt.Pattern, "...}", "}")][strings.ToLower(rt.Method)]
		if !ok {
			t.Errorf("%s %s: not in the spec", rt.Method, rt.Pattern)
			continue
		}
		var op struct {
			Security []map[string][]string `yaml:"security"`
		}
		if err := node.Decode(&op); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, req := range op.Security {
			if s, ok := req["bearerAuth"]; ok {
				got = s
			}
		}
		if len(got) != 1 || got[0] != rt.Scope() {
			t.Errorf("%s %s: spec's bearerAuth scopes %v, route needs %s", rt.Method, rt.Pattern, got, rt.Scope())
		}
	}
}

// With a bearer token: every route refuses a token without its scope (403,
// before looking at the space), and lets one with only that scope through.
// The space belongs to the token's caller ID.
func TestScopeMatrix(t *testing.T) {
	idp := authtest.New(t)
	e := newEnvWithIDP(t, idp)
	all := idp.Token(t, "ada", auth.AllScopes, nil)
	bearer := func(method, path, tok string, body io.Reader, ct string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.Header.Set("Authorization", "Bearer "+tok)
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, r)
		return rec
	}
	rec := bearer("POST", "/v1/spaces", all, strings.NewReader(`{"name":"ada's"}`), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create space with a token: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	sp := created.ID
	if err := e.spaces.Owned(context.Background(), "test:ada", sp); err != nil {
		t.Fatalf("the space must belong to the caller ID test:ada: %v", err)
	}
	// alice (basic auth) can't see it: different owner.
	if rec := e.req("GET", "/v1/spaces/"+sp, "alice", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("alice reading ada's space: %d", rec.Code)
	}

	n := 0
	for _, rt := range Routes(e.deps) {
		if !rt.Auth {
			continue
		}
		n++
		p := strings.NewReplacer("{id}", sp, "{run_id}", "0d0f8a8e-1111-4111-8111-111111111111", "{path...}", "x.txt",
			"{name}", "fn", "{version}", "1").Replace(rt.Pattern)
		var without []string
		for _, s := range auth.AllScopes {
			if s != rt.Scope() {
				without = append(without, s)
			}
		}
		rec := bearer(rt.Method, p, idp.Token(t, "ada", without, nil), nil, "")
		if rec.Code != http.StatusForbidden || decodeError(t, rec).Code != CodeForbidden ||
			!strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="insufficient_scope", scope="`+rt.Scope()+`"`) {
			t.Errorf("%s %s without %s: %d %v %s, want 403 insufficient_scope", rt.Method, p, rt.Scope(), rec.Code, rec.Header(), rec.Body)
		}
		rec = bearer(rt.Method, p, idp.Token(t, "ada", []string{rt.Scope()}, nil), nil, "")
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s with only %s: %d %s", rt.Method, p, rt.Scope(), rec.Code, rec.Body)
		}
	}
	if n < 30 {
		t.Fatalf("only %d authenticated routes", n)
	}
	// A token for another API (wrong audience) is refused outright.
	if rec := bearer("GET", "/v1/spaces", idp.Token(t, "ada", auth.AllScopes, map[string]any{"aud": "api://other"}), nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token for another audience: %d", rec.Code)
	}
}

// Every operation has an HTTP route, and every authenticated route names an
// operation. The gRPC service has the same check, so both transports cover
// the same operations with the same scopes.
func TestRoutesCoverOperations(t *testing.T) {
	used := map[auth.Op]bool{}
	for _, rt := range Routes(newEnv(t).deps) {
		if rt.Auth {
			if rt.Op.Scope() == "" {
				t.Errorf("%s %s: operation %q has no scope", rt.Method, rt.Pattern, rt.Op)
			}
			used[rt.Op] = true
		} else if rt.Op != "" {
			t.Errorf("%s %s: an operation on a route without authentication", rt.Method, rt.Pattern)
		}
	}
	for _, op := range auth.AllOps() {
		if twin := op.RESTTwin(); twin != "" {
			if !used[twin] || twin.Scope() != op.Scope() {
				t.Errorf("operation %s: its REST twin %s has no route or another scope", op, twin)
			}
			continue
		}
		if !used[op] {
			t.Errorf("operation %s has no HTTP route", op)
		}
	}
}
