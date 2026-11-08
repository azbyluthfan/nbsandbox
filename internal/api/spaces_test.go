// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

const nbJSON = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": []}`

// multipartBody builds an upload body. files alternate filename, content.
func multipartBody(t *testing.T, files ...string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i < len(files); i += 2 {
		h := textproto.MIMEHeader{}
		// Written by hand so names with "/" reach the server unchanged.
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, files[i]))
		h.Set("Content-Type", "application/octet-stream")
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = pw.Write([]byte(files[i+1]))
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (e testEnv) req(method, path, user string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if user != "" {
		r.SetBasicAuth(user, user+"-password")
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return rec
}

func (e testEnv) reqWithHeader(method, path, user string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if user != "" {
		r.SetBasicAuth(user, user+"-password")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return rec
}

func (e testEnv) createSpace(t *testing.T, user, name string) spaceJSON {
	t.Helper()
	rec := e.req("POST", "/v1/spaces", user, strings.NewReader(fmt.Sprintf(`{"name": %q}`, name)), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create space: %d %s", rec.Code, rec.Body)
	}
	var sp spaceJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &sp)
	return sp
}

func (e testEnv) upload(t *testing.T, user, spaceID, query string, files ...string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, files...)
	return e.req("POST", "/v1/spaces/"+spaceID+"/files"+query, user, body, ct)
}

func TestSpaceCRUD(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "forecasts")
	if sp.Usage == nil || sp.Usage.QuotaBytes != testMaxSpace || sp.Name != "forecasts" {
		t.Fatalf("created %+v", sp)
	}

	rec := e.req("GET", "/v1/spaces/"+sp.ID, "alice", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"run_count":0`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	rec = e.req("PATCH", "/v1/spaces/"+sp.ID, "alice", strings.NewReader(`{"name":"sales"}`), "application/json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"sales"`) {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	for body, why := range map[string]string{`{}`: "missing name", `{"name":"x","extra":1}`: "unknown field", `nope`: "not JSON"} {
		if rec := e.req("PATCH", "/v1/spaces/"+sp.ID, "alice", strings.NewReader(body), "application/json"); rec.Code != http.StatusBadRequest {
			t.Errorf("patch %s: %d", why, rec.Code)
		}
	}
	// Creating with no body at all is fine (name is optional).
	if rec := e.req("POST", "/v1/spaces", "alice", nil, ""); rec.Code != http.StatusCreated {
		t.Fatalf("create without body: %d %s", rec.Code, rec.Body)
	}

	rec = e.req("DELETE", "/v1/spaces/"+sp.ID, "alice", nil, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/spaces/"+sp.ID, "alice", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", rec.Code)
	}
}

// Pages, stable order, limits validated.
func TestSpaceListPaging(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.createSpace(t, "alice", fmt.Sprintf("s%d", i))
	}
	e.createSpace(t, "bob", "bob's")
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		rec := e.req("GET", "/v1/spaces?limit=2&cursor="+cursor, "alice", nil, "")
		var body struct {
			Items      []spaceJSON `json:"items"`
			NextCursor string      `json:"next_cursor"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		for _, sp := range body.Items {
			if seen[sp.ID] {
				t.Fatalf("space %s on two pages", sp.ID)
			}
			if sp.Usage != nil {
				t.Fatal("list items carry no usage")
			}
			seen[sp.ID] = true
		}
		if body.NextCursor == "" {
			break
		}
		cursor = body.NextCursor
		if page > 5 {
			t.Fatal("paging does not end")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d of alice's spaces, want 5", len(seen))
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=x", "cursor=%21%21"} {
		if rec := e.req("GET", "/v1/spaces?"+q, "alice", nil, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}

func TestFilesFlow(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")

	rec := e.upload(t, "alice", sp.ID, "", "forecast.ipynb", nbJSON, "notes.txt", "hello")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"path":"notes.txt"`) {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if rec := e.upload(t, "alice", sp.ID, "?folder=data", "sales.csv", "a,b\n"); rec.Code != http.StatusCreated {
		t.Fatalf("upload to folder: %d %s", rec.Code, rec.Body)
	}
	if rec := e.upload(t, "alice", sp.ID, "?dir=data", "lost.csv", "a\n"); rec.Code != http.StatusBadRequest {
		t.Fatalf("the old ?dir= must be refused, not ignored: %d %s", rec.Code, rec.Body)
	}

	rec = e.req("GET", "/v1/spaces/"+sp.ID+"/files", "alice", nil, "")
	var list struct {
		Items []struct{ Path, Type, Version string } `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Items) != 4 || list.Items[0].Path != "data" || list.Items[0].Type != "folder" ||
		list.Items[1].Path != "data/sales.csv" || list.Items[1].Type != "file" || list.Items[1].Version == "" {
		t.Fatalf("list: %s", rec.Body)
	}

	rec = e.req("GET", "/v1/spaces/"+sp.ID+"/files/data/sales.csv", "alice", nil, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "a,b\n" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") ||
		rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download headers: %v", rec.Header())
	}

	if rec := e.req("DELETE", "/v1/spaces/"+sp.ID+"/files/notes.txt", "alice", nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete file: %d", rec.Code)
	}
	if rec := e.req("GET", "/v1/spaces/"+sp.ID+"/files/notes.txt", "alice", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("download deleted: %d", rec.Code)
	}
	rec = e.req("GET", "/v1/spaces/"+sp.ID, "alice", nil, "")
	if !strings.Contains(rec.Body.String(), `"file_count":2`) {
		t.Fatalf("usage after delete: %s", rec.Body)
	}
}

// Non-UUID IDs → 400 on every space-scoped route.
func TestNonUUIDIDs(t *testing.T) {
	e := newEnv(t)
	good := "0d0f8a8e-1111-4111-8111-111111111111"
	fill := func(pattern, id, runID string) string {
		return strings.NewReplacer("{id}", id, "{run_id}", runID, "{path...}", "x.txt", "{name}", "fn", "{version}", "1").Replace(pattern)
	}
	for _, rt := range Routes(e.deps) {
		if !rt.SpaceScoped {
			continue
		}
		for _, bad := range []string{"123", "not-a-uuid", "0D0F8A8E-1111-4111-8111-111111111111"} {
			p := fill(rt.Pattern, bad, good)
			if rec := e.req(rt.Method, p, "alice", nil, ""); rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s: %d, want 400", rt.Method, p, rec.Code)
			}
			if rt.RunScoped {
				p := fill(rt.Pattern, good, bad)
				if rec := e.req(rt.Method, p, "alice", nil, ""); rec.Code != http.StatusBadRequest {
					t.Errorf("%s %s: %d, want 400", rt.Method, p, rec.Code)
				}
			}
		}
	}
}

// Bob gets 404 on every space-scoped route
// of alice's space. Generated from the route table, so new routes are covered
// automatically.
func TestAuthorizationMatrix(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "private")
	if rec := e.upload(t, "alice", sp.ID, "", "secret.txt", "SECRET", "nb.ipynb", nbJSON); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	run := e.runToEnd(t, "alice", sp.ID, `{"notebook":"nb.ipynb"}`)
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/functions/fn/versions", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}
	missing := "0d0f8a8e-1111-4111-8111-111111111111"
	n := 0
	for _, rt := range Routes(e.deps) {
		if !rt.SpaceScoped {
			continue
		}
		n++
		for _, id := range []string{sp.ID, missing} {
			p := strings.NewReplacer("{id}", id, "{run_id}", run.ID, "{path...}", "secret.txt",
				"{name}", "fn", "{version}", "1").Replace(rt.Pattern)
			var body io.Reader
			ct := ""
			switch {
			case rt.Method == "POST" && strings.HasSuffix(rt.Pattern, "/runs"):
				body, ct = strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"
			case rt.Method == "POST":
				body, ct = multipartBody(t, "x.txt", "x")
			case rt.Method == "PATCH":
				body, ct = strings.NewReader(`{"name":"pwned"}`), "application/json"
			}
			rec := e.req(rt.Method, p, "bob", body, ct)
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "SECRET") {
				t.Errorf("bob %s %s: %d %s, want 404", rt.Method, p, rec.Code, rec.Body)
			}
		}
	}
	if n < 17 {
		t.Fatalf("only %d space-scoped routes found", n)
	}
	// alice's run is untouched.
	if rec := e.req("GET", "/v1/spaces/"+sp.ID+"/runs/"+run.ID, "alice", nil, ""); !strings.Contains(rec.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("alice's run changed: %s", rec.Body)
	}
	// Nothing changed for alice.
	if rec := e.req("GET", "/v1/spaces/"+sp.ID+"/files/secret.txt", "alice", nil, ""); rec.Body.String() != "SECRET" {
		t.Fatal("alice's file was changed")
	}
	if rec := e.req("GET", "/v1/spaces/"+sp.ID, "alice", nil, ""); !strings.Contains(rec.Body.String(), `"name":"private"`) {
		t.Fatal("alice's space was renamed")
	}
}

// Traversal and symlink paths are rejected; nothing
// outside the space is touched.
func TestTraversalRejected(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	victim := e.createSpace(t, "bob", "")
	if rec := e.upload(t, "bob", victim.ID, "", "canary.txt", "CANARY"); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}

	uploads := map[string]string{
		"filename with ..":       "../" + victim.ID + "/canary.txt",
		"absolute filename":      "/etc/passwd",
		"backslash filename":     `..\x.txt`,
		"internal filename":      ".nbsandbox-status.json",
		"folder with ..":         "?folder=../" + victim.ID,
		"encoded folder with ..": "?folder=%2e%2e%2f" + victim.ID,
	}
	for name, v := range uploads {
		query, filename := "", "pwn.txt"
		if strings.HasPrefix(v, "?") {
			query = v
		} else {
			filename = v
		}
		rec := e.upload(t, "alice", sp.ID, query, filename, "PWNED")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("upload %s: %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	downloads := []string{
		"%2e%2e/" + victim.ID + "/canary.txt", "%2E%2E%2F" + victim.ID + "%2Fcanary.txt",
		"..%5c" + victim.ID,
	}
	for _, p := range downloads {
		rec := e.req("GET", "/v1/spaces/"+sp.ID+"/files/"+p, "alice", nil, "")
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "CANARY") {
			t.Errorf("download %s: %d %s", p, rec.Code, rec.Body)
		}
	}
	// bob's space is intact and alice's has nothing new.
	if rec := e.req("GET", "/v1/spaces/"+victim.ID+"/files/canary.txt", "bob", nil, ""); rec.Body.String() != "CANARY" {
		t.Fatal("victim's file changed")
	}
	var list struct{ Items []spaces.File }
	_ = json.Unmarshal(e.req("GET", "/v1/spaces/"+victim.ID+"/files", "bob", nil, "").Body.Bytes(), &list)
	if len(list.Items) != 1 { // canary.txt
		t.Fatalf("victim's space has extra entries: %v", list.Items)
	}
	if rec := e.req("GET", "/v1/spaces/"+sp.ID+"/files", "alice", nil, ""); strings.Contains(rec.Body.String(), "pwn") {
		t.Fatalf("something was stored: %s", rec.Body)
	}
}

// Over limits → 413 and nothing stored.
func TestUploadLimits(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	big := strings.Repeat("x", testMaxFile+1)
	full := strings.Repeat("x", testMaxFile)

	if rec := e.upload(t, "alice", sp.ID, "", "ok.txt", "fine", "big.txt", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("file over limit: %d %s", rec.Code, rec.Body)
	}
	if rec := e.upload(t, "alice", sp.ID, "", "1", full, "2", full, "3", full, "4", full, "5", "x"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over quota: %d %s", rec.Code, rec.Body)
	}
	// A body larger than the request limit is cut off with 413 too.
	huge := strings.Repeat("x", testMaxSpace+128<<10)
	if rec := e.upload(t, "alice", sp.ID, "", "huge.txt", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge body: %d %s", rec.Code, rec.Body)
	}
	rec := e.req("GET", "/v1/spaces/"+sp.ID, "alice", nil, "")
	if !strings.Contains(rec.Body.String(), `"bytes_used":0`) || !strings.Contains(rec.Body.String(), `"file_count":0`) {
		t.Fatalf("something was stored: %s", rec.Body)
	}
	var b ErrorBody
	_ = json.Unmarshal(e.upload(t, "alice", sp.ID, "", "big.txt", big).Body.Bytes(), &b)
	if b.Error.Code != CodePayloadTooLarge {
		t.Fatalf("error code %q", b.Error.Code)
	}
}

// A non-notebook submitted as a notebook → 400.
func TestInvalidNotebookUpload(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	for name, body := range map[string]string{"text": "print('hi')", "nbformat 3": `{"nbformat":3,"cells":[]}`, "no cells": `{"nbformat":4}`} {
		rec := e.upload(t, "alice", sp.ID, "", "x.ipynb", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "notebook") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestUploadRequiresMultipart(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/files", "alice", strings.NewReader("x"), "text/plain"); rec.Code != http.StatusBadRequest {
		t.Fatalf("plain body: %d", rec.Code)
	}
	// A part with the wrong field name.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("other", "x")
	_ = mw.Close()
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/files", "alice", &buf, mw.FormDataContentType()); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong part: %d %s", rec.Code, rec.Body)
	}
	// No files at all.
	var empty bytes.Buffer
	mw = multipart.NewWriter(&empty)
	_ = mw.Close()
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/files", "alice", &empty, mw.FormDataContentType()); rec.Code != http.StatusBadRequest {
		t.Fatalf("no parts: %d %s", rec.Code, rec.Body)
	}
}
