// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/ui"
)

func uiEnv(t *testing.T) (testEnv, http.Handler) {
	t.Helper()
	e := newEnv(t)
	return e, checkSpec(t, NewUIHandler(e.deps, ui.Static()), e.deps.Users.Has)
}

func uiReq(h http.Handler, method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestUIUsersAndStatic(t *testing.T) {
	_, h := uiEnv(t)
	rec := uiReq(h, "GET", "/ui/api/users", nil, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"users":["alice","bob","root"]}` {
		t.Fatalf("users: %d %s", rec.Code, rec.Body)
	}
	rec = uiReq(h, "GET", "/", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<script type="module" src="app.js">`) ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("index: %d %v", rec.Code, rec.Header())
	}
	for path, ct := range map[string]string{"/app.js": "javascript", "/app.css": "text/css", "/run.js": "javascript"} {
		if rec := uiReq(h, "GET", path, nil, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), ct) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := uiReq(h, "GET", "/nope.js", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset: %d", rec.Code)
	}
}

// The UI path acts as the chosen user, under the API's own rules.
func TestUIActsAsChosenUser(t *testing.T) {
	e, h := uiEnv(t)
	rec := uiReq(h, "POST", "/ui/api/as/alice/v1/spaces", strings.NewReader(`{"name":"via ui"}`), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create as alice: %d %s", rec.Code, rec.Body)
	}
	var sp spaceJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &sp)
	// The space really belongs to alice (seen through the password API).
	if rec := e.req("GET", "/v1/spaces/"+sp.ID, "alice", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice via API: %d", rec.Code)
	}
	if rec := uiReq(h, "GET", "/ui/api/as/alice/v1/spaces", nil, ""); !strings.Contains(rec.Body.String(), sp.ID) {
		t.Fatalf("list as alice: %s", rec.Body)
	}
	if rec := uiReq(h, "GET", "/ui/api/as/bob/v1/spaces", nil, ""); strings.Contains(rec.Body.String(), sp.ID) {
		t.Fatalf("bob sees alice's space: %s", rec.Body)
	}
	for _, p := range []string{"/ui/api/as/mallory/v1/spaces", "/ui/api/as/alice/healthz", "/ui/api/as/alice/metrics", "/ui/api/whatever"} {
		if rec := uiReq(h, "GET", p, nil, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	// The main API still needs a password.
	if rec := e.req("GET", "/v1/spaces", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("main API without password: %d", rec.Code)
	}
	// The UI paths don't exist on the main API.
	if rec := e.req("GET", "/ui/api/as/alice/v1/spaces", "", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("UI path on the main API: %d", rec.Code)
	}
}

// The authorization matrix again, through the UI path: acting as bob, every
// space-scoped route on alice's space is 404.
func TestUIAuthorizationMatrix(t *testing.T) {
	e, h := uiEnv(t)
	sp := e.createSpace(t, "alice", "private")
	e.upload(t, "alice", sp.ID, "", "secret.txt", "SECRET", "nb.ipynb", nbJSON)
	run := e.runToEnd(t, "alice", sp.ID, `{"notebook":"nb.ipynb"}`)
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/functions/fn/versions", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}
	n := 0
	for _, rt := range Routes(e.deps) {
		if !rt.SpaceScoped {
			continue
		}
		n++
		file := "secret.txt"
		if strings.Contains(rt.Pattern, "/artifacts/") {
			file = "summary.csv"
		}
		p := UIPrefix + strings.NewReplacer("{id}", sp.ID, "{run_id}", run.ID, "{path...}", file,
			"{name}", "fn", "{version}", "1").Replace(rt.Pattern)
		p = strings.Replace(p, "{user}", "bob", 1)
		var body io.Reader
		ct := ""
		switch {
		case rt.Method == "POST" && strings.HasSuffix(rt.Pattern, "/runs"):
			body, ct = strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"
		case rt.Method == "POST" && strings.HasSuffix(rt.Pattern, "/files"):
			body, ct = multipartBody(t, "x.txt", "x")
		case rt.Method == "PATCH":
			body, ct = strings.NewReader(`{"name":"pwned"}`), "application/json"
		}
		rec := uiReq(h, rt.Method, p, body, ct)
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("as bob %s %s: %d", rt.Method, p, rec.Code)
		}
		alice := strings.Replace(p, "/as/bob/", "/as/alice/", 1)
		// Streams are covered by their own tests (the space's status stream
		// never ends by design).
		if rt.Method == "GET" && !strings.HasSuffix(rt.Pattern, "/stream") && !strings.HasSuffix(rt.Pattern, "/events") {
			if rec := uiReq(h, "GET", alice, nil, ""); rec.Code != http.StatusOK {
				t.Errorf("as alice GET %s: %d", alice, rec.Code)
			}
		}
	}
	if n < 18 {
		t.Fatalf("only %d space-scoped routes", n)
	}
}

// The UI runs a notebook and follows the live log the way the browser does
// (EventSource on the UI path, no Authorization header).
func TestUIRunAndStream(t *testing.T) {
	e, h := uiEnv(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "lines.ipynb", nbJSON)
	base := "/ui/api/as/alice/v1/spaces/" + sp.ID
	rec := uiReq(h, "POST", base+"/runs", strings.NewReader(`{"notebook":"lines.ipynb"}`), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var v runs.View
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	resp, err := srv.Client().Get(srv.URL + base + "/runs/" + v.ID + "/log/stream")
	if err != nil {
		t.Fatal(err)
	}
	c := &sseClient{resp: resp, r: newReader(resp.Body)}
	defer func() { _ = resp.Body.Close() }()
	lines, end := c.readAll(t)
	checkSequence(t, lines, 1)
	if len(lines) != 32 || end.Name != "done" || !strings.Contains(end.Data, "succeeded") {
		t.Fatalf("%d lines, end %+v", len(lines), end)
	}
	if rec := uiReq(h, "GET", base+"/runs/"+v.ID+"/output.html", nil, ""); !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("rendered notebook must stay sandboxed through the UI: %v", rec.Header())
	}
}
