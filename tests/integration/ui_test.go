// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var uiURL = envOr("NBSANDBOX_UI_URL", "http://127.0.0.1:8090")

func uiCall(t *testing.T, method, path string, body io.Reader, ct string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, uiURL+path, body)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("UI %s %s: %v (is the web UI enabled?)", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// Everything the browser does, through the UI port, without a
// password: users, a space, an upload, a run, and its live log.
func TestUIPortEndToEnd(t *testing.T) {
	if clustered {
		t.Skip("the cluster stack has no web UI")
	}
	code, body, hdr := uiCall(t, "GET", "/", nil, "")
	if code != http.StatusOK || !strings.Contains(string(body), "nbsandbox") || !strings.Contains(hdr.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("index: %d %v", code, hdr)
	}
	code, body, _ = uiCall(t, "GET", "/ui/api/users", nil, "")
	if code != http.StatusOK || !strings.Contains(string(body), `"alice"`) {
		t.Fatalf("users: %d %s", code, body)
	}

	base := "/ui/api/as/alice/v1"
	code, body, _ = uiCall(t, "POST", base+"/spaces", strings.NewReader(`{"name":"ui e2e"}`), "application/json")
	if code != http.StatusCreated {
		t.Fatalf("create space: %d %s", code, body)
	}
	var sp struct{ ID string }
	_ = json.Unmarshal(body, &sp)
	t.Cleanup(func() {
		uiCall(t, "DELETE", base+"/spaces/"+sp.ID, nil, "")
		assertNoRunContainers(t)
	})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	nb, _ := os.ReadFile(filepath.Join(repoRoot(), "tests", "workloads", "stream.ipynb"))
	w, _ := mw.CreateFormFile("file", "stream.ipynb")
	_, _ = w.Write(nb)
	_ = mw.Close()
	if code, body, _ := uiCall(t, "POST", base+"/spaces/"+sp.ID+"/files", &buf, mw.FormDataContentType()); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, body)
	}

	// Bob, through the UI, can't see it; the main API still wants a password.
	if code, body, _ := uiCall(t, "GET", "/ui/api/as/bob/v1/spaces/"+sp.ID, nil, ""); code != http.StatusNotFound {
		t.Fatalf("bob via UI: %d %s", code, body)
	}
	if r := call(t, "GET", "/v1/spaces", "", nil, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("main API without password: %d", r.Code)
	}

	code, body, _ = uiCall(t, "POST", base+"/spaces/"+sp.ID+"/runs", strings.NewReader(`{"notebook":"stream.ipynb"}`), "application/json")
	if code != http.StatusCreated {
		t.Fatalf("run: %d %s", code, body)
	}
	var run struct{ ID string }
	_ = json.Unmarshal(body, &run)

	// The live log, as EventSource requests it (no Authorization header).
	resp, err := (&http.Client{}).Get(uiURL + base + "/spaces/" + sp.ID + "/runs/" + run.ID + "/log/stream")
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{resp: resp, r: bufio.NewReaderSize(resp.Body, 64<<10)}
	defer s.close()
	lines, end, err := s.readToEnd()
	if err != nil {
		t.Fatal(err)
	}
	var first time.Time
	for _, ev := range lines {
		if strings.HasPrefix(ev.Data, "line 0") {
			first = ev.Received
		}
	}
	ok := end.Name == "done" && strings.Contains(end.Data, "succeeded") && sequential(lines, 1) == nil &&
		!first.IsZero() && end.Received.Sub(first) > 8*time.Second
	finding(t, "web-ui", "run a notebook and follow its log through the local web UI port", "live lines, then done; other users still get 404",
		fmt.Sprintf("%d lines, first line %.1fs before done, %s", len(lines), end.Received.Sub(first).Seconds(), end.Data), ok)
	if !ok {
		t.Fatalf("%d lines, end %+v", len(lines), end)
	}
}
