// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/auth/authtest"
)

func TestAdminSpaces(t *testing.T) {
	e := newEnv(t)
	a1 := e.createSpace(t, "alice", "one")
	e.createSpace(t, "alice", "two")
	b1 := e.createSpace(t, "bob", "bobs")

	// Basic users without admin: true get 403 on every admin route.
	for _, u := range []string{"alice", "bob"} {
		for _, r := range [][2]string{{"GET", "/v1/admin/spaces"}, {"PATCH", "/v1/admin/spaces/" + a1.ID}, {"DELETE", "/v1/admin/spaces/" + b1.ID}} {
			if rec := e.req(r[0], r[1], u, strings.NewReader(`{"owner":"`+u+`"}`), "application/json"); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s: %d", u, r[0], r[1], rec.Code)
			}
		}
	}

	list := func(q string) adminList {
		t.Helper()
		rec := e.req("GET", "/v1/admin/spaces"+q, "root", nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s: %d %s", q, rec.Code, rec.Body)
		}
		var l adminList
		_ = json.Unmarshal(rec.Body.Bytes(), &l)
		return l
	}
	if l := list(""); len(l.Items) != 3 || l.Items[0].Owner != "bob" || l.Items[2].Owner != "alice" {
		t.Fatalf("all: %+v", l)
	}
	if l := list("?owner=alice&limit=1"); len(l.Items) != 1 || l.NextCursor == "" || l.Items[0].Owner != "alice" {
		t.Fatalf("alice's first page: %+v", l)
	}
	if rec := e.req("GET", "/v1/admin/spaces?limit=0", "root", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0: %d", rec.Code)
	}

	// Give alice's space to bob.
	rec := e.req("PATCH", "/v1/admin/spaces/"+a1.ID, "root", strings.NewReader(`{"owner":"bob"}`), "application/json")
	var moved adminSpaceJSON
	if _ = json.Unmarshal(rec.Body.Bytes(), &moved); rec.Code != http.StatusOK || moved.Owner != "bob" || moved.Usage == nil {
		t.Fatalf("set owner: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/spaces/"+a1.ID, "alice", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("alice still sees it: %d", rec.Code)
	}
	if rec := e.req("GET", "/v1/spaces/"+a1.ID, "bob", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("bob doesn't see it: %d", rec.Code)
	}
	for body, want := range map[string]int{`{"owner":""}`: 400, `{"owner":"x","name":"y"}`: 400, `not json`: 400} {
		if rec := e.req("PATCH", "/v1/admin/spaces/"+a1.ID, "root", strings.NewReader(body), "application/json"); rec.Code != want {
			t.Errorf("PATCH %s: %d, want %d", body, rec.Code, want)
		}
	}
	missing := "0d0f8a8e-1111-4111-8111-111111111111"
	for _, r := range [][3]string{{"PATCH", missing, "404"}, {"DELETE", missing, "404"}, {"PATCH", "nope", "400"}, {"DELETE", "NOPE", "400"}} {
		rec := e.req(r[0], "/v1/admin/spaces/"+r[1], "root", strings.NewReader(`{"owner":"bob"}`), "application/json")
		if want := r[2]; http.StatusText(rec.Code) != http.StatusText(map[string]int{"404": 404, "400": 400}[want]) {
			t.Errorf("%s %s: %d, want %s", r[0], r[1], rec.Code, want)
		}
	}

	// Delete bob's space.
	if rec := e.req("DELETE", "/v1/admin/spaces/"+b1.ID, "root", nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/spaces/"+b1.ID, "bob", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete: %d", rec.Code)
	}
	if !strings.Contains(e.logs.String(), "space deleted by an administrator") {
		t.Fatal("the delete isn't logged")
	}
}

type adminList struct {
	Items      []adminSpaceJSON `json:"items"`
	NextCursor string           `json:"next_cursor"`
}

// Tokens: nbsandbox.admin is never implied, and the run records the token's
// client application.
func TestAdminScopeAndClientApp(t *testing.T) {
	idp := authtest.New(t)
	e := newEnvWithIDP(t, idp)
	bearer := func(method, path, tok string) int {
		return e.reqWithHeader(method, path, "", nil, map[string]string{"Authorization": "Bearer " + tok}).Code
	}
	everything := idp.Token(t, "ada", []string{auth.ScopeRead, auth.ScopeSpacesWrite, auth.ScopeFilesWrite, auth.ScopeRuns}, nil)
	if c := bearer("GET", "/v1/admin/spaces", everything); c != http.StatusForbidden {
		t.Fatalf("without the admin scope: %d", c)
	}
	admin := idp.Token(t, "ops", []string{auth.ScopeAdmin}, nil)
	if c := bearer("GET", "/v1/admin/spaces", admin); c != http.StatusOK {
		t.Fatalf("with the admin scope: %d", c)
	}
	if c := bearer("GET", "/v1/spaces", admin); c != http.StatusForbidden {
		t.Fatalf("admin alone reads its own spaces: %d", c)
	}

	rec := e.reqWithHeader("POST", "/v1/spaces", "", strings.NewReader(`{}`), map[string]string{"Authorization": "Bearer " + everything, "Content-Type": "application/json"})
	var sp spaceJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &sp)
	tok := idp.Token(t, "ada", []string{auth.ScopeFilesWrite, auth.ScopeRuns, auth.ScopeRead}, map[string]any{"azp": "reporting-app"})
	body, ct := multipartBody(t, "nb.ipynb", nbJSON)
	if rec := e.reqWithHeader("POST", "/v1/spaces/"+sp.ID+"/files", "", body, map[string]string{"Authorization": "Bearer " + tok, "Content-Type": ct}); rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	rec = e.reqWithHeader("POST", "/v1/spaces/"+sp.ID+"/runs", "", strings.NewReader(`{"notebook":"nb.ipynb"}`),
		map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"client":"reporting-app"`) {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	// Basic auth: no client.
	a := e.createSpace(t, "alice", "")
	if rec := e.upload(t, "alice", a.ID, "", "nb.ipynb", nbJSON); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	if rec := e.req("POST", "/v1/spaces/"+a.ID+"/runs", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"); strings.Contains(rec.Body.String(), `"client"`) {
		t.Fatalf("a basic-auth run has a client: %s", rec.Body)
	}
}
