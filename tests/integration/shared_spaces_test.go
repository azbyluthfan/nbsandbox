// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
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
	// Before binding: refused when submitted, saying which name to bind;
	// alice's connections aren't his to bind.
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "bob", map[string]any{"notebook": "read.ipynb"}, nil)
	expectRefused(t, "connections-shared_unbound", "a member runs a notebook without binding their own connections (the owner's are bound)",
		fmt.Sprintf("HTTP %d %s", r.Code, strings.TrimSpace(string(r.Body))), r.Code == http.StatusBadRequest && strings.Contains(string(r.Body), "crm"))
	var names struct {
		Bound map[string]map[string][]string `json:"bound"`
	}
	var aliceSpace struct {
		Environments map[string]map[string]struct{ ID string } `json:"environments"`
	}
	call(t, "GET", "/v1/spaces/"+sp+"/connections", "alice", nil, nil).json(t, &aliceSpace)
	steal := jsonCall(t, "PUT", "/v1/spaces/"+sp+"/connections", "bob", map[string]any{"environments": map[string]any{
		"dev": map[string]string{"crm": aliceSpace.Environments["dev"]["crm"].ID}}}, nil)
	expectRefused(t, "connections-shared_others_connection", "a member binds another member's connection to a name",
		fmt.Sprintf("HTTP %d", steal.Code), steal.Code == http.StatusBadRequest)
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
	call(t, "GET", "/v1/spaces/"+sp+"/names", "alice", nil, nil).json(t, &names)
	if strings.Join(names.Bound["bob"]["dev"], ",") != "crm,lake" {
		t.Errorf("bob's bound names: %+v", names.Bound)
	}
	if g := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID, "alice", nil, nil); g.Code != http.StatusOK || !strings.Contains(string(g.Body), `"owner":"bob"`) {
		t.Errorf("alice reads bob's run: %d %s", g.Code, g.Body)
	}
}

// What a member of a shared space can't do, whatever their role, for the
// isolation report (docs/sharing.md).
func TestSharedSpaceAccess(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"nb.ipynb": codeNotebook("import time; time.sleep(30)")})
	base := "/v1/spaces/" + sp
	code := func(method, path, user string, body any) int {
		t.Helper()
		if body == nil {
			return call(t, method, path, user, nil, nil).Code
		}
		return jsonCall(t, method, path, user, body, nil).Code
	}

	got := code("GET", base, "bob", nil)
	expectRefused(t, "shared-no-role", "a caller without a role reads a space", fmt.Sprintf("HTTP %d", got), got == http.StatusNotFound)

	if c := code("PUT", base+"/members/bob", "alice", map[string]string{"role": "read"}); c != http.StatusOK {
		t.Fatalf("grant: %d", c)
	}
	up := call(t, "POST", base+"/files", "bob", strings.NewReader("--x--\r\n"), map[string]string{"Content-Type": "multipart/form-data; boundary=x"}).Code
	run := code("POST", base+"/runs", "bob", map[string]string{"notebook": "nb.ipynb"})
	expectRefused(t, "shared-read-writes", "a member with read uploads a file and starts a run",
		fmt.Sprintf("upload HTTP %d, run HTTP %d", up, run), up == http.StatusForbidden && run == http.StatusForbidden)

	// write: their own runs and sessions; not another member's.
	if c := code("PUT", base+"/members/bob", "alice", map[string]string{"role": "write"}); c != http.StatusOK {
		t.Fatalf("grant write: %d", c)
	}
	alices := startRun(t, "alice", sp, "nb.ipynb", map[string]any{})
	cancel := code("POST", base+"/runs/"+alices.ID+"/cancel", "bob", map[string]string{})
	expectRefused(t, "shared-others-run", "a member with write cancels another member's run",
		fmt.Sprintf("HTTP %d", cancel), cancel == http.StatusForbidden)
	call(t, "POST", base+"/runs/"+alices.ID+"/cancel", "alice", nil, nil)

	r := jsonCall(t, "POST", base+"/sessions", "alice", map[string]string{"notebook": "nb.ipynb", "mode": "read-only"}, nil)
	if r.Code != http.StatusCreated && r.Code != http.StatusOK {
		t.Fatalf("alice's session: %d %s", r.Code, r.Body)
	}
	var s struct{ ID string }
	r.json(t, &s)
	t.Cleanup(func() { call(t, "DELETE", base+"/sessions/"+s.ID, "alice", nil, nil) })
	peek := code("GET", base+"/sessions/"+s.ID, "bob", nil)
	interrupt := code("POST", base+"/sessions/"+s.ID+"/interrupt", "bob", nil)
	expectRefused(t, "shared-others-session", "a member reads and interrupts another member's session",
		fmt.Sprintf("get HTTP %d, interrupt HTTP %d", peek, interrupt), peek == http.StatusNotFound && interrupt == http.StatusNotFound)

	// Revoked: the space is gone for them again.
	if c := code("DELETE", base+"/members/bob", "alice", nil); c != http.StatusNoContent {
		t.Fatalf("revoke: %d", c)
	}
	after := code("GET", base+"/runs", "bob", nil)
	expectRefused(t, "shared-revoked", "a revoked member lists the space's runs", fmt.Sprintf("HTTP %d", after), after == http.StatusNotFound)
}
