// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/runs"
)

const reportNB = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": [
 {"cell_type": "code", "source": "import nbsandbox\nfor r in range(3):\n    nbsandbox.emit({'n': r})", "metadata": {}, "outputs": []}]}`

// readStream reads a whole SSE response.
func readStream(t *testing.T, c *sseClient) []sseEvent {
	t.Helper()
	var out []sseEvent
	for {
		ev, err := c.next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func names(evs []sseEvent) string {
	var n []string
	for _, e := range evs {
		n = append(n, e.Name)
	}
	return strings.Join(n, ",")
}

// The result of a streamed run: as SSE (by Accept header or ?stream=1),
// item by item then the result; as JSON, the same items as a list.
func TestResultStream(t *testing.T) {
	e, srv, sp := streamEnv(t)
	e.upload(t, "alice", sp, "", "report.ipynb", reportNB)
	v := e.startRun(t, sp, "report.ipynb")
	base := "/v1/spaces/" + sp + "/runs/" + v.ID

	req, _ := http.NewRequest("GET", srv.URL+base+"/result?include=progress", nil)
	req.SetBasicAuth("alice", "alice-password")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %v %v", resp, err)
	}
	evs := readStream(t, &sseClient{resp: resp, r: newReader(resp.Body)})
	_ = resp.Body.Close()
	if names(evs) != "item,progress,item,progress,item,progress,result" || evs[0].ID != 1 || evs[4].ID != 3 {
		t.Fatalf("events %s %+v", names(evs), evs)
	}
	var it runs.RunEvent
	_ = json.Unmarshal([]byte(evs[2].Data), &it)
	if it.Index != 2 || *it.Total != 3 || string(it.Item) != `{"n":2}` {
		t.Fatalf("item %+v", it)
	}
	var res runs.CallerResult
	_ = json.Unmarshal([]byte(evs[6].Data), &res)
	if res.Status != "succeeded" || string(res.Output) != `[{"n":1},{"n":2},{"n":3}]` || *res.Items != 3 {
		t.Fatalf("result %+v %s", res, res.Output)
	}
	// JSON: the same list.
	rec := e.req("GET", base+"/result", "alice", nil, "")
	var j runs.CallerResult
	_ = json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != 200 || string(j.Output) != string(res.Output) {
		t.Fatalf("JSON %d %s", rec.Code, rec.Body)
	}
	// ?stream=1 after the run, resuming after item 2: a replay.
	c := openStream(t, srv, base+"/result?stream=1", "alice", 2)
	if got := readStream(t, c); names(got) != "item,result" || got[0].ID != 3 {
		t.Fatalf("resumed replay %s", names(got))
	}
	for _, bad := range []string{"?stream=1&include=cell", "?stream=1&after=-1"} {
		if rec := e.req("GET", base+"/result"+bad, "alice", nil, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec := e.req("GET", base+"/result?stream=1", "bob", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob: %d", rec.Code)
	}
	// The events stream: status only, unless asked.
	if got := readStream(t, openStream(t, srv, base+"/events", "alice", 0)); names(got) != "status,status,status" {
		t.Fatalf("events %s", names(got))
	}
	if got := readStream(t, openStream(t, srv, base+"/events?include=progress", "alice", 0)); strings.Count(names(got), "progress") != 3 {
		t.Fatalf("events with progress %s", names(got))
	}
	if rec := e.req("GET", base+"/events?include=item", "alice", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("include=item: %d", rec.Code)
	}
}

// Publishing with a contract in the request; invoking as a stream.
func TestPublishContractAndInvokeStream(t *testing.T) {
	e, srv, sp := streamEnv(t)
	e.upload(t, "alice", sp, "", "report.ipynb", reportNB)
	base := "/v1/spaces/" + sp + "/functions/regions"
	rec := e.req("POST", base+"/versions", "alice", strings.NewReader(`{"notebook": "report.ipynb",
		"contract": {"output": {"mode": "stream", "item": {"type": "object"}}}}`), "application/json")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"mode":"stream"`) || !strings.Contains(rec.Body.String(), `"mode_inferred":false`) {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}
	rec = e.req("POST", base+"/versions", "alice", strings.NewReader(`{"notebook": "report.ipynb",
		"contract": {"output": {"mode": "single", "schema": {}}}}`), "application/json")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "nbsandbox.emit") {
		t.Fatalf("contradicting contract: %d %s", rec.Code, rec.Body)
	}
	req, _ := http.NewRequest("POST", srv.URL+base+"/invoke?stream=1", strings.NewReader(`{"parameters": {}}`))
	req.SetBasicAuth("alice", "alice-password")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := readStream(t, &sseClient{resp: resp, r: newReader(resp.Body)}); names(got) != "item,item,item,result" {
		t.Fatalf("invoke stream %s", names(got))
	}
}
