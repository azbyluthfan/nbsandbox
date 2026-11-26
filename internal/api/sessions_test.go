// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// openSession opens alice's session on nb.ipynb and returns its ID.
func (e testEnv) openSession(t *testing.T, spaceID string) string {
	t.Helper()
	rec := e.req("POST", "/v1/spaces/"+spaceID+"/sessions", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json")
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("open session: %d %s", rec.Code, rec.Body)
	}
	var s SessionView
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	return s.ID
}

func dial(t *testing.T, srv *httptest.Server, path, user string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if header == nil {
		header = http.Header{}
	}
	if user != "" {
		req, _ := http.NewRequest("GET", "http://x", nil)
		req.SetBasicAuth(user, user+"-password")
		header.Set("Authorization", req.Header.Get("Authorization"))
	}
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, header)
}

func readJSON(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, b, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return m
}

func msgType(m map[string]any) string {
	h, _ := m["header"].(map[string]any)
	s, _ := h["msg_type"].(string)
	return s
}

func content(m map[string]any) map[string]any {
	c, _ := m["content"].(map[string]any)
	return c
}

func TestSessionLifecycleOverREST(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "s")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON)
	base := "/v1/spaces/" + sp.ID + "/sessions"
	for _, body := range []string{`{"notebook":"missing.ipynb"}`, `{"notebook":"nb.ipynb","mode":"sideways"}`, `{}`, `{"notebook":"nb.ipynb","x":1}`} {
		if rec := e.req("POST", base, "alice", strings.NewReader(body), "application/json"); rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	id := e.openSession(t, sp.ID)
	// The same notebook: the same session.
	rec := e.req("POST", base, "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("again: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", base, "alice", nil, ""); !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("list: %s", rec.Body)
	}
	// It holds the write lock: no writing run meanwhile.
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/runs", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "session") {
		t.Fatalf("run during a read-write session: %d %s", rec.Code, rec.Body)
	}
	for _, action := range []string{"interrupt", "restart"} {
		if rec := e.req("POST", base+"/"+id+"/"+action, "alice", nil, ""); rec.Code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body)
		}
	}
	if rec := e.req("POST", base+"/"+id+"/sync", "alice", nil, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"conflicts":[]`) {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/admin/sessions", "root", nil, ""); !strings.Contains(rec.Body.String(), `"owner":"alice"`) {
		t.Fatalf("admin list: %s", rec.Body)
	}
	if rec := e.req("GET", "/v1/admin/sessions", "alice", nil, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("admin list as alice: %d", rec.Code)
	}
	rec = e.req("DELETE", base+"/"+id, "alice", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ended"`) || !strings.Contains(rec.Body.String(), `"ended_by":"alice"`) {
		t.Fatalf("end: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("POST", base+"/"+id+"/interrupt", "alice", nil, ""); rec.Code != http.StatusConflict {
		t.Fatalf("interrupt an ended session: %d %s", rec.Code, rec.Body)
	}
	// Audited: the start and the end, with the notebook.
	rec = e.req("GET", "/v1/admin/audit?space="+sp.ID, "root", nil, "")
	for _, want := range []string{`"action":"OpenSession"`, `"action":"EndSession"`, `"action":"InterruptSession"`, "nb.ipynb (read-write)"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("audit lacks %s: %s", want, rec.Body)
		}
	}
}

func TestAdminEndsAnySession(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "s")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON)
	id := e.openSession(t, sp.ID)
	rec := e.req("POST", "/v1/admin/sessions/"+id+"/end", "root", strings.NewReader(`{"reason":"maintenance"}`), "application/json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ended by an administrator: maintenance") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestDeletingASpaceEndsItsSessions(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "s")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON)
	id := e.openSession(t, sp.ID)
	if rec := e.req("DELETE", "/v1/spaces/"+sp.ID, "alice", nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if s, err := e.deps.Sessions.All(context.Background(), ""); err != nil || len(s) != 0 {
		t.Fatalf("open sessions after the space went: %v %v (session %s)", s, err, id)
	}
}

// The WebSocket: Jupyter messages both ways; refusals answered; the end
// announced.
func TestSessionChannel(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.handler)
	defer srv.Close()
	sp := e.createSpace(t, "alice", "s")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON)
	id := e.openSession(t, sp.ID)
	path := "/v1/spaces/" + sp.ID + "/sessions/" + id + "/channel"

	c, resp, err := dial(t, srv, path, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	defer func() { _ = c.Close() }()
	hello := readJSON(t, c)
	if hello["channel"] != "bridge" || content(hello)["status"] != "attached" {
		t.Fatalf("hello %v", hello)
	}
	exec := `{"channel":"shell","header":{"msg_id":"m1","msg_type":"execute_request"},"content":{"code":"print(1)"}}`
	if err := c.WriteMessage(websocket.TextMessage, []byte(exec)); err != nil {
		t.Fatal(err)
	}
	for {
		m := readJSON(t, c)
		if msgType(m) == "stream" {
			if content(m)["text"] != "print(1)" {
				t.Fatalf("stream %v", m)
			}
			break
		}
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"channel":"shell","header":{"msg_id":"m2","msg_type":"history_request"},"content":{}}`)); err != nil {
		t.Fatal(err)
	}
	for {
		m := readJSON(t, c)
		if m["channel"] == "bridge" && content(m)["refused"] == true {
			if !strings.Contains(content(m)["error"].(string), "history_request") { //nolint:errcheck,forcetypeassert // a test: a panic is a failure
				t.Fatalf("refusal %v", m)
			}
			break
		}
	}
	// Ending it: a last "ended" message, then the close.
	if rec := e.req("DELETE", "/v1/spaces/"+sp.ID+"/sessions/"+id, "alice", nil, ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	for {
		m := readJSON(t, c)
		if content(m)["status"] == "ended" {
			break
		}
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := c.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("after the end: %v", err)
	}
}

func TestSessionChannelRefusals(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.handler)
	defer srv.Close()
	sp := e.createSpace(t, "alice", "s")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON)
	id := e.openSession(t, sp.ID)
	path := "/v1/spaces/" + sp.ID + "/sessions/" + id + "/channel"
	cases := []struct {
		name, user string
		header     http.Header
		want       int
	}{
		{"no credentials", "", nil, http.StatusUnauthorized},
		{"someone else", "bob", nil, http.StatusNotFound},
		{"another site's page", "alice", http.Header{"Origin": {"https://evil.example"}}, http.StatusForbidden},
	}
	for _, tc := range cases {
		_, resp, err := dial(t, srv, path, tc.user, tc.header)
		if err == nil || resp == nil || resp.StatusCode != tc.want {
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			t.Errorf("%s: %v %d, want %d", tc.name, err, code, tc.want)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	// This site's own page may.
	host := strings.TrimPrefix(srv.URL, "http://")
	c, resp, err := dial(t, srv, path, "alice", http.Header{"Origin": {"http://" + host}})
	if err != nil {
		t.Fatalf("same origin: %v", err)
	}
	_ = resp.Body.Close()
	_ = c.Close()
	// Not a WebSocket.
	if rec := e.req("GET", path, "alice", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("plain GET: %d", rec.Code)
	}
}

func TestSessionsOff(t *testing.T) {
	e := newEnv(t)
	e.deps.Sessions = nil
	h := NewHandler(e.deps)
	sp := e.createSpace(t, "alice", "s")
	r := httptest.NewRequest("GET", "/v1/spaces/"+sp.ID+"/sessions", nil)
	r.SetBasicAuth("alice", "alice-password")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "turned off") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestSessionViewHidesOwnerFromOwners(t *testing.T) {
	v := sessionView(store.Session{ID: spaces.NewID(), Owner: "alice", Worker: "w1", Status: store.SessionReady}, false)
	if v.Owner != "" || v.Worker != "" || v.EndedAt != nil {
		t.Fatalf("%+v", v)
	}
}
