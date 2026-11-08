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

func TestFunctionsAPI(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON, "lines.ipynb", nbJSON, "r.ipynb", rNotebook)
	base := "/v1/spaces/" + sp.ID + "/functions/"
	post := func(path, body string) (int, map[string]any) {
		rec := e.req("POST", base+path, "alice", strings.NewReader(body), "application/json")
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, v := post("quick/versions", `{"notebook": "nb.ipynb"}`); code != http.StatusCreated || v["version"] != float64(1) {
		t.Fatalf("publish: %d %v", code, v)
	}
	if code, _ := post("BAD/versions", `{"notebook": "nb.ipynb"}`); code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", code)
	}
	if code, v := post("rfn/versions", `{"notebook": "r.ipynb"}`); code != http.StatusBadRequest ||
		!strings.Contains(fmt.Sprint(v["error"]), "Python notebooks only") {
		t.Fatalf("publishing an R notebook: %d %v", code, v)
	}
	// With wait, the answer is the result itself.
	start := time.Now()
	code, res := post("quick/invoke?wait=10s", `{"parameters": {"cfg": {"a": [1, null]}}}`)
	if code != http.StatusOK || res["status"] != "succeeded" || res["function"] != "quick" || time.Since(start) > 5*time.Second {
		t.Fatalf("invoke and wait: %d %v", code, res)
	}
	// The wait ends before a slow run does: 202 with the run, then the
	// result endpoint waits for the rest.
	if code, _ := post("slow/versions", `{"notebook": "lines.ipynb"}`); code != http.StatusCreated {
		t.Fatal("publish slow")
	}
	code, run := post("slow/invoke?wait=50ms", ``)
	if code != http.StatusAccepted || run["id"] == nil || (run["status"] != "queued" && run["status"] != "running") {
		t.Fatalf("short wait: %d %v", code, run)
	}
	rec := e.req("GET", "/v1/spaces/"+sp.ID+"/runs/"+fmt.Sprint(run["id"])+"/result?wait=10s", "alice", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("result wait: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/spaces/"+sp.ID+"/runs/"+fmt.Sprint(run["id"])+"/result?wait=2m", "alice", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("wait over the maximum: %d", rec.Code)
	}

	rec = e.req("GET", base+"quick/contract", "alice", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"input":null`) {
		t.Fatalf("contract: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/spaces/"+sp.ID+"/functions", "alice", nil, ""); !strings.Contains(rec.Body.String(), `"name":"quick"`) {
		t.Fatalf("list: %s", rec.Body)
	}
	if rec := e.req("DELETE", base+"quick", "alice", nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", base+"quick", "alice", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete: %d", rec.Code)
	}
}
