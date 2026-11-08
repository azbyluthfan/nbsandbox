// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFileEditingAPI(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	base := "/v1/spaces/" + sp.ID

	put := func(path, body string, h map[string]string) (int, map[string]any, http.Header) {
		rec := e.reqWithHeader("PUT", base+"/files/"+path, "alice", strings.NewReader(body), h)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Header()
	}
	code, created, hdr := put("src/model.py", "x = 1\n", map[string]string{"If-None-Match": "*"})
	if code != http.StatusCreated || created["version"] == "" || hdr.Get("ETag") != `"`+fmt.Sprint(created["version"])+`"` {
		t.Fatalf("create: %d %v %v", code, created, hdr)
	}
	// The editor opens the file: the ETag is the version it works on.
	get := e.req("GET", base+"/files/src/model.py", "alice", nil, "")
	etag := get.Header().Get("ETag")
	if get.Code != http.StatusOK || etag == "" {
		t.Fatalf("download: %d %v", get.Code, get.Header())
	}
	if code, body, _ := put("src/model.py", "x = 2\n", nil); code != http.StatusPreconditionRequired ||
		!strings.Contains(fmt.Sprint(body["error"]), CodeVersionRequired) {
		t.Fatalf("save without If-Match: %d %v", code, body)
	}
	time.Sleep(2 * time.Millisecond)
	code, saved, _ := put("src/model.py", "x = 2\n", map[string]string{"If-Match": etag})
	if code != http.StatusOK {
		t.Fatalf("save: %d %v", code, saved)
	}
	// A second tab still on the first version gets 412 and the current one.
	code, conflict, hdr := put("src/model.py", "x = 3\n", map[string]string{"If-Match": etag})
	if code != http.StatusPreconditionFailed || conflict["current_version"] != saved["version"] ||
		hdr.Get("ETag") != `"`+fmt.Sprint(saved["version"])+`"` {
		t.Fatalf("stale save: %d %v %v", code, conflict, hdr)
	}

	// Folders, move, recursive delete.
	if rec := e.req("POST", base+"/folders", "alice", strings.NewReader(`{"path": "data/2026"}`), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("folder: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("POST", base+"/folders", "alice", strings.NewReader(`{"path": "data/2026"}`), "application/json"); rec.Code != http.StatusConflict {
		t.Fatalf("existing folder: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("PATCH", base+"/files/src", "alice", strings.NewReader(`{"path": "data/2026/src"}`), "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("move folder: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", base+"/files/data/2026/src/model.py", "alice", nil, ""); rec.Body.String() != "x = 2\n" {
		t.Fatalf("after move: %d %q", rec.Code, rec.Body)
	}
	if rec := e.req("DELETE", base+"/files/data", "alice", nil, ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete non-empty folder: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("DELETE", base+"/files/data?recursive=yes", "alice", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad recursive value: %d", rec.Code)
	}
	if rec := e.req("DELETE", base+"/files/data?recursive=true", "alice", nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("recursive delete: %d %s", rec.Code, rec.Body)
	}
	rec := e.req("GET", base+"/files", "alice", nil, "")
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("after delete: %s", rec.Body)
	}
	// Bob: 404 for everything, whatever the body.
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/files/x.txt", "x"}, {"PATCH", "/files/x.txt", `{"path":"y"}`}, {"POST", "/folders", `{"path":"y"}`},
	} {
		if rec := e.req(c.method, base+c.path, "bob", strings.NewReader(c.body), "application/json"); rec.Code != http.StatusNotFound {
			t.Errorf("bob %s %s: %d", c.method, c.path, rec.Code)
		}
	}
}
