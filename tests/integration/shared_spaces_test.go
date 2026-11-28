// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A shared space, end to end (make test-connections): alice declares crm
// and binds her connection; bob, a member with write, reads with his own
// (docs/plan-shared-spaces.md).
func TestConnectionsSharedSpace(t *testing.T) {
	needConnections(t)
	sp := readerSpace(t, codeNotebook(`import nbsandbox
n = int(nbsandbox.sql("SELECT count(*) AS n FROM crm.orders").n.iloc[0])
nbsandbox.output({"n": n})`))
	if r := jsonCall(t, "PUT", "/v1/spaces/"+sp+"/members/bob", "alice", map[string]string{"role": "write"}, nil); r.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", r.Code, r.Body)
	}
	// Before binding: refused when submitted, saying which name to bind.
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "bob", map[string]any{"notebook": "read.ipynb"}, nil)
	if r.Code != http.StatusBadRequest || !strings.Contains(string(r.Body), "crm") {
		t.Fatalf("bob's run before binding: %d %s", r.Code, r.Body)
	}
	// bob's own connection (a wrong password in prod, like alice's), bound to crm.
	var c, lake struct{ ID string }
	cr := jsonCall(t, "POST", "/v1/connections", "bob", map[string]any{"name": "bobs-crm", "type": "postgresql", "settings": pgSettings(),
		"secret": map[string]any{"value": readerPass}}, nil)
	if cr.Code != http.StatusCreated {
		t.Fatalf("bob's connection: %d %s", cr.Code, cr.Body)
	}
	cr.json(t, &c)
	lr := jsonCall(t, "POST", "/v1/connections", "bob", map[string]any{"name": "bobs-lake", "type": "s3", "settings": s3Settings(),
		"secret": map[string]any{"value": s3Secret}}, nil)
	if lr.Code != http.StatusCreated {
		t.Fatalf("bob's lake: %d %s", lr.Code, lr.Body)
	}
	lr.json(t, &lake)
	if br := jsonCall(t, "PUT", "/v1/spaces/"+sp+"/connections", "bob", map[string]any{"environments": map[string]any{
		"dev": map[string]string{"crm": "bobs-crm", "lake": "bobs-lake"}}}, nil); br.Code != http.StatusOK {
		t.Fatalf("bob binds: %d %s", br.Code, br.Body)
	}
	t.Cleanup(func() {
		call(t, "DELETE", "/v1/spaces/"+sp+"/members/bob", "alice", nil, nil) // drops his bindings
		call(t, "DELETE", "/v1/connections/"+c.ID, "bob", nil, nil)
		call(t, "DELETE", "/v1/connections/"+lake.ID, "bob", nil, nil)
	})
	run := startRun(t, "bob", sp, "read.ipynb", nil)
	res := runResult(t, "bob", sp, run.ID)
	if res.Status != "succeeded" {
		t.Fatalf("bob's run: %+v", res)
	}
	var out struct{ N int }
	_ = json.Unmarshal(res.Output, &out)
	if out.N != 1000 {
		t.Errorf("bob read %d orders", out.N)
	}
	// alice sees bob's run, and that he has bound crm and lake in dev.
	var names struct {
		Bound map[string]map[string][]string `json:"bound"`
	}
	call(t, "GET", "/v1/spaces/"+sp+"/names", "alice", nil, nil).json(t, &names)
	if strings.Join(names.Bound["bob"]["dev"], ",") != "crm,lake" {
		t.Errorf("bob's bound names: %+v", names.Bound)
	}
	if g := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID, "alice", nil, nil); g.Code != http.StatusOK || !strings.Contains(string(g.Body), `"owner":"bob"`) {
		t.Errorf("alice reads bob's run: %d %s", g.Code, g.Body)
	}
}
