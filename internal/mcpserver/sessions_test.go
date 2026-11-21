// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
)

// fakeSessions is a session's REST calls and its channel, answering like
// the session bridge: a state message first, then Jupyter messages.
type fakeSessions struct {
	mu    sync.Mutex
	paths []string
	auth  []string
}

var pngBytes = []byte("\x89PNG fake image")

func (f *fakeSessions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/spaces/s1/sessions":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "k1", "notebook": body["notebook"], "mode": body["mode"], "status": "starting"})
	case r.Method == "DELETE" && r.URL.Path == "/v1/spaces/s1/sessions/k1":
		_, _ = w.Write([]byte(`{"id":"k1","status":"ended"}`))
	case r.Method == "POST" && r.URL.Path == "/v1/spaces/s1/sessions/k1/interrupt":
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.Method == "GET" && r.URL.Path == "/v1/spaces/s1/sessions/k1/channel":
		f.channel(w, r)
	case r.Method == "GET" && r.URL.Path == "/v1/spaces/s1/sessions/gone/channel":
		http.Error(w, `{"error":{"code":"not_found","message":"no such session"}}`, http.StatusNotFound)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSessions) channel(w http.ResponseWriter, r *http.Request) {
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close() }()
	send := func(channel, typ, parent string, content map[string]any) {
		_ = ws.WriteJSON(map[string]any{"channel": channel, "header": map[string]any{"msg_type": typ},
			"parent_header": map[string]any{"msg_id": parent}, "content": content})
	}
	send("bridge", "state", "", map[string]any{"status": "ready", "kernel": "idle", "mode": "read-write"})
	for {
		var m struct {
			Header  map[string]any `json:"header"`
			Content map[string]any `json:"content"`
		}
		if ws.ReadJSON(&m) != nil {
			return
		}
		id, _ := m.Header["msg_id"].(string)
		code, _ := m.Content["code"].(string)
		switch m.Header["msg_type"] {
		case "complete_request":
			send("iopub", "status", "other", map[string]any{"execution_state": "busy"}) // another client's: ignored
			send("shell", "complete_reply", id, map[string]any{"matches": []string{"amount", "month"}, "cursor_start": 4, "cursor_end": 4})
		case "execute_request":
			send("iopub", "status", id, map[string]any{"execution_state": "busy"})
			send("iopub", "execute_input", id, map[string]any{"execution_count": 7})
			switch {
			case strings.Contains(code, "sleep"):
				continue // never idle
			case strings.Contains(code, "queued"):
				send("iopub", "status", id, map[string]any{"execution_state": "idle"})
				send("shell", "execute_reply", id, map[string]any{"status": "aborted"}) // after idle, as it can be
				continue
			case strings.Contains(code, "boom"):
				send("iopub", "error", id, map[string]any{"ename": "ZeroDivisionError", "evalue": "division by zero",
					"traceback": []string{"\x1b[31mTraceback\x1b[0m", "\x1b[1;31mZeroDivisionError\x1b[0m: division by zero"}})
			default:
				send("iopub", "stream", id, map[string]any{"name": "stdout", "text": "hello\n"})
				send("iopub", "display_data", id, map[string]any{"data": map[string]any{
					"image/png": base64.StdEncoding.EncodeToString(pngBytes), "text/plain": "<Figure>"}})
				send("iopub", "execute_result", id, map[string]any{"data": map[string]any{"text/plain": "42"}})
			}
			send("iopub", "status", id, map[string]any{"execution_state": "idle"})
			send("shell", "execute_reply", id, map[string]any{"status": "ok"})
		}
	}
}

func connectSessions(t *testing.T, f *fakeSessions) *mcp.ClientSession {
	t.Helper()
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	s := New(Options{Client: &apiclient.Client{BaseURL: api.URL, HTTP: api.Client()}, Identity: &apiclient.Identity{Token: "tok"}, Version: "test"})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestSessionTools(t *testing.T) {
	f := &fakeSessions{}
	cs := connectSessions(t, f)

	if res, text := callTool(t, cs, "open_session", map[string]any{"space_id": "s1", "notebook": "nb.ipynb", "read_only": true}); res.IsError ||
		!strings.Contains(text, `"mode":"read-only"`) || !strings.Contains(text, `"id":"k1"`) {
		t.Fatalf("open_session: %s", text)
	}

	res, text := callTool(t, cs, "run_cell", map[string]any{"space_id": "s1", "session_id": "k1", "code": "print('hello'); 42"})
	if res.IsError || !strings.HasPrefix(text, `{"execution_count":7,"status":"ok"}`) || !strings.Contains(text, "hello\n42\n") {
		t.Errorf("run_cell: %q", text)
	}
	var img *mcp.ImageContent
	for _, c := range res.Content {
		if i, ok := c.(*mcp.ImageContent); ok {
			img = i
		}
	}
	if img == nil || string(img.Data) != string(pngBytes) || img.MIMEType != "image/png" || strings.Contains(text, "<Figure>") {
		t.Errorf("the chart: %v (text %q)", img, text)
	}

	res, text = callTool(t, cs, "run_cell", map[string]any{"space_id": "s1", "session_id": "k1", "code": "1/0 # boom"})
	if !res.IsError || !strings.Contains(text, `"status":"error"`) || !strings.Contains(text, "ZeroDivisionError: division by zero") || strings.Contains(text, "\x1b") {
		t.Errorf("a failing cell: %v %q", res.IsError, text)
	}

	_, text = callTool(t, cs, "run_cell", map[string]any{"space_id": "s1", "session_id": "k1", "code": "time.sleep(99)", "wait_seconds": 1})
	if !strings.Contains(text, `"status":"running"`) || !strings.Contains(text, "interrupt_session") {
		t.Errorf("a long cell: %q", text)
	}

	res, text = callTool(t, cs, "run_cell", map[string]any{"space_id": "s1", "session_id": "k1", "code": "x # queued"})
	if !res.IsError || !strings.Contains(text, `"status":"aborted"`) || !strings.Contains(text, "run it again") {
		t.Errorf("an aborted cell: %v %q", res.IsError, text)
	}

	_, text = callTool(t, cs, "complete_code", map[string]any{"space_id": "s1", "session_id": "k1", "code": `df["`})
	if !strings.Contains(text, `"matches":["amount","month"]`) {
		t.Errorf("complete_code: %s", text)
	}

	for _, tool := range []string{"interrupt_session", "end_session"} {
		if res, text := callTool(t, cs, tool, map[string]any{"space_id": "s1", "session_id": "k1"}); res.IsError {
			t.Errorf("%s: %s", tool, text)
		}
	}

	res, text = callTool(t, cs, "run_cell", map[string]any{"space_id": "s1", "session_id": "gone", "code": "1"})
	if !res.IsError || !strings.Contains(text, "list_sessions") {
		t.Errorf("a session that's gone: %q", text)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.auth {
		if a != "Bearer tok" {
			t.Errorf("auth %q", a)
		}
	}
}
