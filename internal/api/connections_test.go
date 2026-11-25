// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/connections"
)

func (e testEnv) createConnection(t *testing.T, user, body string) connections.View {
	t.Helper()
	rec := e.req(http.MethodPost, "/v1/connections", user, strings.NewReader(body), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create connection: %d %s", rec.Code, rec.Body)
	}
	var v connections.View
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConnectionsAPI(t *testing.T) {
	e := newEnv(t)
	dev := e.createConnection(t, "alice", `{"name":"crm-dev","type":"postgresql","settings":{"host":"pg-dev.internal","database":"crm","user":"reader"},"secret":{"value":"dev-SECRET"}}`)
	prod := e.createConnection(t, "alice", `{"name":"crm-prod","type":"postgresql","settings":{"host":"pg.internal","database":"crm","user":"reader"},"secret":{"value":"prod-SECRET"}}`)
	if dev.Secret.Mode != "managed" || dev.Settings["port"] != float64(5432) {
		t.Fatalf("created: %+v", dev)
	}

	// Refused: a host not allowed, a missing secret, an unknown field, a taken name.
	for body, code := range map[string]int{
		`{"name":"x","type":"postgresql","settings":{"host":"db.example.com","database":"d","user":"u"},"secret":{"value":"v"}}`:    400,
		`{"name":"x","type":"postgresql","settings":{"host":"pg.internal","database":"d","user":"u"}}`:                              400,
		`{"name":"x","type":"postgresql","settings":{"host":"pg.internal","database":"d","user":"u"},"secrt":{"value":"v"}}`:        400,
		`{"name":"crm-dev","type":"postgresql","settings":{"host":"pg.internal","database":"d","user":"u"},"secret":{"value":"v"}}`: 409,
	} {
		if rec := e.req(http.MethodPost, "/v1/connections", "alice", strings.NewReader(body), "application/json"); rec.Code != code {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}

	// A space's names, per environment.
	sp := e.createSpace(t, "alice", "forecast")
	rec := e.req(http.MethodPut, "/v1/spaces/"+sp.ID+"/connections", "alice",
		strings.NewReader(`{"default_environment":"dev","environments":{"dev":{"crm":"crm-dev"},"prod":{"crm":"crm-prod"}}}`), "application/json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"crm-prod"`) {
		t.Fatalf("set the space's connections: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req(http.MethodGet, "/v1/spaces/"+sp.ID+"/connections", "alice", nil, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"default_environment":"dev"`) {
		t.Fatalf("get the space's connections: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req(http.MethodGet, "/v1/connections/"+prod.ID, "alice", nil, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"used_by"`) {
		t.Fatalf("get with its uses: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req(http.MethodDelete, "/v1/connections/"+prod.ID, "alice", nil, ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete a bound connection: %d %s", rec.Code, rec.Body)
	}

	// Bob sees none of it.
	for _, r := range []struct{ method, path string }{
		{"GET", "/v1/connections/" + prod.ID}, {"PATCH", "/v1/connections/" + prod.ID}, {"DELETE", "/v1/connections/" + prod.ID},
		{"GET", "/v1/spaces/" + sp.ID + "/connections"}, {"PUT", "/v1/spaces/" + sp.ID + "/connections"},
	} {
		if rec := e.req(r.method, r.path, "bob", strings.NewReader(`{}`), "application/json"); rec.Code != http.StatusNotFound {
			t.Errorf("bob %s %s: %d", r.method, r.path, rec.Code)
		}
	}
	if rec := e.req(http.MethodGet, "/v1/connections", "bob", nil, ""); rec.Code != 200 || strings.Contains(rec.Body.String(), "crm") {
		t.Fatalf("bob's list: %s", rec.Body)
	}

	// Rotate the secret; no response ever carries a secret's value.
	if rec := e.req(http.MethodPatch, "/v1/connections/"+dev.ID, "alice", strings.NewReader(`{"secret":{"value":"new-SECRET"}}`), "application/json"); rec.Code != 200 {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v1/connections", "/v1/connections/" + dev.ID, "/v1/spaces/" + sp.ID + "/connections", "/v1/admin/audit?limit=100"} {
		user := "alice"
		if strings.HasPrefix(path, "/v1/admin") {
			user = "root"
		}
		if rec := e.req(http.MethodGet, path, user, nil, ""); strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("%s shows a secret: %s", path, rec.Body)
		}
	}
	if strings.Contains(e.logs.String(), "SECRET") {
		t.Error("the logs show a secret")
	}
	// The changes are in the audit log, the secret isn't.
	if page := e.audit(t, "?action=UpdateConnection"); len(page.Items) == 0 || page.Items[0].Target != dev.ID {
		t.Fatalf("audit: %+v", page.Items)
	}

	// Unbind everything, then delete.
	if rec := e.req(http.MethodPut, "/v1/spaces/"+sp.ID+"/connections", "alice", strings.NewReader(`{"environments":{}}`), "application/json"); rec.Code != 200 {
		t.Fatalf("unbind: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req(http.MethodDelete, "/v1/connections/"+prod.ID, "alice", nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req(http.MethodGet, "/v1/connections/not-a-uuid", "alice", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestConnectionsTurnedOff(t *testing.T) {
	e := newEnv(t)
	e.deps.Connections = nil
	h := NewHandler(e.deps)
	for _, path := range []string{"/v1/connections"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth("alice", "alice-password")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "turned off") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}
