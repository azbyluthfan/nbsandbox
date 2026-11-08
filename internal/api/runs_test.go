// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
)

// fakeBackend writes what the runner would: output notebook, HTML, an
// artifact and the status file. A run with "slow" in its notebook name waits
// to be cancelled.
type fakeBackend struct{}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Orphans(context.Context) ([]isolation.Orphan, error) {
	return nil, nil
}
func (f *fakeBackend) Kill(context.Context, string) error { return nil }
func (f *fakeBackend) Run(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
	dir := spec.RunDir
	out.Line("running " + spec.Notebook)
	_ = os.WriteFile(filepath.Join(dir, "output.ipynb"), []byte(nbJSON), 0o600)
	if strings.Contains(spec.Notebook, "lines") { // 30 lines, 20 ms apart
		for i := 1; i <= 30; i++ {
			out.Line(fmt.Sprintf("tick %d", i))
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				return isolation.RunResult{Stopped: true, ExitCode: 143}, nil
			}
		}
	}
	if strings.Contains(spec.Notebook, "report") { // a streamed output: three items, a little apart
		for i := 1; i <= 3; i++ {
			f, _ := os.OpenFile(filepath.Join(dir, ".nbsandbox-events.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
			_, _ = fmt.Fprintf(f, `{"kind":"item","item":{"n":%d},"total":3}`+"\n"+`{"kind":"progress","fraction":%.2f}`+"\n", i, float64(i)/3)
			_ = f.Close()
			time.Sleep(30 * time.Millisecond)
		}
		_ = os.WriteFile(filepath.Join(dir, ".nbsandbox-status.json"), []byte(`{"schema_version":2,"state":"succeeded","rendered":true,"cgroup":{}}`), 0o600)
		return isolation.RunResult{}, nil
	}
	if strings.Contains(spec.Notebook, "slow") {
		<-ctx.Done()
		_ = os.WriteFile(filepath.Join(dir, ".nbsandbox-status.json"), []byte(`{"schema_version":2,"state":"terminated","cgroup":{}}`), 0o600)
		return isolation.RunResult{Stopped: true, ExitCode: 143}, nil
	}
	_ = os.WriteFile(filepath.Join(dir, "output.html"), []byte("<html><script>alert(1)</script></html>"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "artifacts", "summary.csv"), []byte("a,b\n1,2\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ".nbsandbox-status.json"), []byte(`{"schema_version":2,"state":"succeeded","rendered":true,"cgroup":{}}`), 0o600)
	return isolation.RunResult{}, nil
}

// runToEnd submits a run and waits for a terminal status.
func (e testEnv) runToEnd(t *testing.T, user, spaceID, body string) runs.View {
	t.Helper()
	rec := e.req("POST", "/v1/spaces/"+spaceID+"/runs", user, strings.NewReader(body), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create run: %d %s", rec.Code, rec.Body)
	}
	var v runs.View
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return e.waitRun(t, user, spaceID, v.ID)
}

func (e testEnv) waitRun(t *testing.T, user, spaceID, runID string) runs.View {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var v runs.View
		rec := e.req("GET", "/v1/spaces/"+spaceID+"/runs/"+runID, user, nil, "")
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		if v.Status != runs.StatusQueued && v.Status != runs.StatusRunning {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("run stuck: %s", rec.Body)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunEndpoints(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON, "slow.ipynb", nbJSON)
	base := "/v1/spaces/" + sp.ID + "/runs"

	v := e.runToEnd(t, "alice", sp.ID, `{"notebook":"nb.ipynb","parameters":{"region":"jakarta"}}`)
	if v.Status != runs.StatusSucceeded || v.Result == nil || !v.Result.Rendered {
		t.Fatalf("run %+v", v)
	}
	run := base + "/" + v.ID

	rec := e.req("GET", run+"/output.html", "alice", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("html: %d %v", rec.Code, rec.Header())
	}
	rec = e.req("GET", run+"/output.ipynb", "alice", nil, "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(rec.Body.String(), "nbformat") {
		t.Fatalf("notebook: %d %v", rec.Code, rec.Header())
	}
	rec = e.req("GET", run+"/log", "alice", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "running nb.ipynb") || !strings.Contains(rec.Body.String(), "[nbsandbox] run finished: succeeded") {
		t.Fatalf("log: %d %q", rec.Code, rec.Body)
	}
	rec = e.req("GET", run+"/artifacts", "alice", nil, "")
	if !strings.Contains(rec.Body.String(), `"path":"summary.csv"`) {
		t.Fatalf("artifacts: %s", rec.Body)
	}
	if rec := e.req("GET", run+"/artifacts/summary.csv", "alice", nil, ""); rec.Body.String() != "a,b\n1,2\n" {
		t.Fatalf("artifact: %q", rec.Body)
	}
	for _, p := range []string{"/artifacts/../output.ipynb", "/artifacts/%2e%2e/log.txt", "/artifacts/missing.csv"} {
		if rec := e.req("GET", run+p, "alice", nil, ""); rec.Code == http.StatusOK {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}

	// List with filters.
	rec = e.req("GET", base+"?status=succeeded&limit=1", "alice", nil, "")
	if !strings.Contains(rec.Body.String(), v.ID) {
		t.Fatalf("list: %s", rec.Body)
	}
	for _, q := range []string{"status=weird", "created_after=yesterday", "limit=0"} {
		if rec := e.req("GET", base+"?"+q, "alice", nil, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("list %s: %d", q, rec.Code)
		}
	}
	if rec := e.req("GET", base+"?created_after=2099-01-01T00:00:00Z", "alice", nil, ""); !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("future filter: %s", rec.Body)
	}

	// A second run while one is active; deleting an active run or space.
	slow := e.req("POST", base, "alice", strings.NewReader(`{"notebook":"slow.ipynb"}`), "application/json")
	var sv runs.View
	_ = json.Unmarshal(slow.Body.Bytes(), &sv)
	if rec := e.req("POST", base, "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"); rec.Code != http.StatusConflict {
		t.Fatalf("second active run: %d", rec.Code)
	}
	if rec := e.req("DELETE", base+"/"+sv.ID, "alice", nil, ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete active run: %d", rec.Code)
	}
	if rec := e.req("DELETE", "/v1/spaces/"+sp.ID, "alice", nil, ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete space with active run: %d", rec.Code)
	}
	if rec := e.req("POST", base+"/"+sv.ID+"/cancel", "alice", nil, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if got := e.waitRun(t, "alice", sp.ID, sv.ID); got.Status != runs.StatusCancelled || !got.Result.Partial {
		t.Fatalf("cancelled run: %+v", got)
	}
	if rec := e.req("POST", base+"/"+sv.ID+"/cancel", "alice", nil, ""); rec.Code != http.StatusConflict {
		t.Fatalf("cancel finished run: %d", rec.Code)
	}
	if rec := e.req("DELETE", run, "alice", nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete run: %d", rec.Code)
	}
	if rec := e.req("GET", run, "alice", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted run: %d", rec.Code)
	}
}

// Invalid run requests, and idempotency.
// rNotebook is a notebook written for the R kernel.
const rNotebook = `{"nbformat": 4, "nbformat_minor": 5, "cells": [],
  "metadata": {"kernelspec": {"name": "ir", "display_name": "R", "language": "R"}}}`

func TestCreateRunValidationAndIdempotency(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON, "data.csv", "a,b", "r.ipynb", rNotebook)
	base := "/v1/spaces/" + sp.ID + "/runs"
	for body, why := range map[string]string{
		`{}`:                           "no notebook",
		`{"notebook":"r.ipynb"}`:       "not Python",
		`{"notebook":"data.csv"}`:      "not a notebook",
		`{"notebook":"missing.ipynb"}`: "missing",
		`{"notebook":"nb.ipynb","parameters":[1]}`:                                                             "params not object",
		`{"notebook":"nb.ipynb","parameters":{"a":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}}`: "params nested too deep",
		`{"notebook":"nb.ipynb","parameters":{"not-python":1}}`:                                                "params bad name",
		`{"notebook":"nb.ipynb","extra":1}`:                                                                    "unknown field",
	} {
		if rec := e.req("POST", base, "alice", strings.NewReader(body), "application/json"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", why, rec.Code, rec.Body)
		}
	}

	post := func(key, body string) (int, runs.View) {
		rr := e.reqWithHeader("POST", base, "alice", strings.NewReader(body), map[string]string{"Content-Type": "application/json", "Idempotency-Key": key})
		var v runs.View
		_ = json.Unmarshal(rr.Body.Bytes(), &v)
		return rr.Code, v
	}
	code, first := post("key-1", `{"notebook":"nb.ipynb","parameters":{"a":1}}`)
	if code != http.StatusCreated {
		t.Fatalf("first: %d", code)
	}
	code, again := post("key-1", `{"parameters":{"a":1},"notebook":"nb.ipynb"}`) // same request, keys reordered
	if code != http.StatusOK || again.ID != first.ID {
		t.Fatalf("replay: %d %s vs %s", code, again.ID, first.ID)
	}
	if code, _ := post("key-1", `{"notebook":"nb.ipynb","parameters":{"a":2}}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("reused key, different body: %d", code)
	}
}

func TestDockerDownGives503(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON)
	e.runs.SetDockerUp(false)
	rec := e.req("POST", "/v1/spaces/"+sp.ID+"/runs", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json")
	if rec.Code != http.StatusServiceUnavailable || decodeError(t, rec).Code != CodeDockerUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("submit while docker down: %d %s", rec.Code, rec.Body)
	}
}

func TestRuntimeEndpoint(t *testing.T) {
	e := newEnv(t)
	rec := e.req("GET", "/v1/runtime", "alice", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"python_version":"3.13.18"`) || !strings.Contains(rec.Body.String(), `"pandas":"3.1.1"`) {
		t.Fatalf("runtime: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/runtime", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("runtime without auth: %d", rec.Code)
	}
}

// A caller that hangs up right after sending a complete
// submission still gets its run recorded and executed.
func TestSubmitSurvivesCallerDisconnect(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the connection is already gone when the handler runs
	req := httptest.NewRequestWithContext(ctx, "POST", "/v1/spaces/"+sp.ID+"/runs", strings.NewReader(`{"notebook":"nb.ipynb"}`))
	req.SetBasicAuth("alice", "alice-password")
	req.Header.Set("Content-Type", "application/json")
	e.handler.ServeHTTP(httptest.NewRecorder(), req)

	var list struct{ Items []runs.View }
	_ = json.Unmarshal(e.req("GET", "/v1/spaces/"+sp.ID+"/runs", "alice", nil, "").Body.Bytes(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("run not recorded after disconnect: %+v", list.Items)
	}
	if v := e.waitRun(t, "alice", sp.ID, list.Items[0].ID); v.Status != runs.StatusSucceeded {
		t.Fatalf("status %s", v.Status)
	}
}
