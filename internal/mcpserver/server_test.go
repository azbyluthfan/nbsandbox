// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
)

// fakeAPI is the slice of nbsandbox's API the tools call.
type fakeAPI struct {
	mu        sync.Mutex
	auth      []string
	paths     []string
	upload    string // the last uploaded file: folder/name and content
	resultHit int    // GET .../result calls
	env       string // the last run's environment
	doneAfter int    // the result is ready from this call on
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.paths = append(f.paths, r.Method+" "+r.URL.RequestURI())
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/spaces":
		_, _ = w.Write([]byte(`{"items":[{"id":"s1","name":"demo"}]}`))
	case r.Method == "POST" && r.URL.Path == "/v1/spaces/s1/files":
		_ = r.ParseMultipartForm(1 << 20)
		fh := r.MultipartForm.File["file"][0]
		file, _ := fh.Open()
		b, _ := io.ReadAll(file)
		f.upload = r.URL.Query().Get("folder") + "/" + fh.Filename + ":" + string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"files":[{"path":"x"}]}`))
	case r.Method == "POST" && r.URL.Path == "/v1/spaces/s1/runs":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.env, _ = body["environment"].(string)
		if body["notebook"] != "nb.ipynb" || body["space"] != "read-only" {
			http.Error(w, `{"error":{"code":"bad","message":"unexpected body"}}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"r1","status":"queued"}`))
	case r.Method == "GET" && r.URL.Path == "/v1/spaces/s1/runs/r1/result":
		f.resultHit++
		if f.resultHit < f.doneAfter {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"r1","status":"running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"succeeded","output":{"total":42}}`))
	case r.Method == "GET" && r.URL.Path == "/v1/spaces/s1/runs/r1":
		_, _ = w.Write([]byte(`{"id":"r1","status":"running"}`))
	case r.Method == "GET" && r.URL.Path == "/v1/spaces/s1/runs/r1/log":
		_, _ = w.Write([]byte(strings.Repeat("x", MaxText+10) + "THE END"))
	case r.Method == "GET" && r.URL.Path == "/v1/spaces/nope/files":
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such space"}}`))
	default:
		http.NotFound(w, r)
	}
}

func connect(t *testing.T, f *fakeAPI, who *apiclient.Identity) *mcp.ClientSession {
	t.Helper()
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	s := New(Options{Client: &apiclient.Client{BaseURL: api.URL, HTTP: api.Client()}, Identity: who, Version: "test"})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return res, text.String()
}

func TestToolsAreListedWithHints(t *testing.T) {
	cs := connect(t, &fakeAPI{}, &apiclient.Identity{Token: "tok"})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"nbsandbox_runtime", "list_spaces", "create_space", "list_files", "read_file", "write_file",
		"run_notebook", "get_run_result", "get_run", "list_runs", "get_run_log", "cancel_run", "list_artifacts", "read_artifact",
		"list_functions", "get_function", "invoke_function",
		"open_session", "list_sessions", "run_cell", "complete_code", "interrupt_session", "end_session"} {
		tool := byName[name]
		if tool == nil || tool.Description == "" || tool.Annotations == nil {
			t.Errorf("tool %s missing or undescribed", name)
		}
	}
	if !byName["list_spaces"].Annotations.ReadOnlyHint || byName["write_file"].Annotations.ReadOnlyHint ||
		!*byName["cancel_run"].Annotations.DestructiveHint {
		t.Error("read-only and destructive hints")
	}
}

func TestRunNotebookWaitsForTheResult(t *testing.T) {
	f := &fakeAPI{doneAfter: 2} // still running on the first wait
	cs := connect(t, f, &apiclient.Identity{Token: "tok"})
	_, text := callTool(t, cs, "run_notebook", map[string]any{"space_id": "s1", "notebook": "nb.ipynb", "read_only": true,
		"parameters": map[string]any{"n": 3}, "environment": "prod"})
	if !strings.Contains(text, `"finished":true`) || !strings.Contains(text, `"total":42`) {
		t.Fatalf("result %s", text)
	}
	if f.env != "prod" {
		t.Errorf("environment %q, want prod", f.env)
	}
	if f.resultHit != 2 {
		t.Errorf("result asked %d times, want 2", f.resultHit)
	}
	for _, a := range f.auth {
		if a != "Bearer tok" {
			t.Errorf("auth %q", a)
		}
	}
}

func TestRunNotebookReturnsARunStillGoing(t *testing.T) {
	f := &fakeAPI{doneAfter: 100}
	cs := connect(t, f, &apiclient.Identity{Token: "tok"})
	_, text := callTool(t, cs, "run_notebook", map[string]any{"space_id": "s1", "notebook": "nb.ipynb", "read_only": true, "wait_seconds": 0})
	if !strings.Contains(text, `"finished":false`) || !strings.Contains(text, "get_run_result") {
		t.Fatalf("result %s", text)
	}
}

func TestWriteFileUsesItsFolder(t *testing.T) {
	f := &fakeAPI{}
	cs := connect(t, f, &apiclient.Identity{Name: "alice", Password: "pw"})
	if res, text := callTool(t, cs, "write_file", map[string]any{"space_id": "s1", "path": "data/in put.csv", "content": "a,b\n1,2\n"}); res.IsError {
		t.Fatal(text)
	}
	if f.upload != "data/in put.csv:a,b\n1,2\n" {
		t.Errorf("uploaded %q", f.upload)
	}
	if !strings.HasPrefix(f.auth[0], "Basic ") {
		t.Errorf("auth %q", f.auth[0])
	}
}

func TestErrorsAndLongTextAreReadable(t *testing.T) {
	cs := connect(t, &fakeAPI{}, &apiclient.Identity{Token: "tok"})
	res, text := callTool(t, cs, "list_files", map[string]any{"space_id": "nope"})
	if !res.IsError || !strings.Contains(text, "no such space") {
		t.Errorf("error %v %s", res.IsError, text)
	}
	_, text = callTool(t, cs, "get_run_log", map[string]any{"space_id": "s1", "run_id": "r1"})
	if !strings.HasSuffix(text, "THE END") || !strings.HasPrefix(text, "[17 earlier bytes not shown]") {
		t.Errorf("log %.60q ... %q", text, text[len(text)-10:])
	}
}

// Over HTTP, with no identity of its own, each call carries its caller's
// Authorization header to nbsandbox.
func TestCallerCredentialsPassThrough(t *testing.T) {
	f := &fakeAPI{}
	api := httptest.NewServer(f)
	defer api.Close()
	s := New(Options{Client: &apiclient.Client{BaseURL: api.URL, HTTP: api.Client()}, Version: "test"})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	srv := httptest.NewServer(h)
	defer srv.Close()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: authTransport{"Bearer caller-token"}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	if res, text := callTool(t, cs, "list_spaces", nil); res.IsError || !strings.Contains(text, "demo") {
		t.Fatalf("list_spaces: %s", text)
	}
	if len(f.auth) != 1 || f.auth[0] != "Bearer caller-token" {
		t.Errorf("nbsandbox saw %q", f.auth)
	}
}

type authTransport struct{ header string }

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", a.header)
	return http.DefaultTransport.RoundTrip(r)
}
