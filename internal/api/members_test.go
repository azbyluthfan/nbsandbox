// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/runs"
)

// Shared spaces over HTTP: each role against each kind of endpoint, and
// the members endpoints (docs/plan-shared-spaces.md).
func TestSharedSpace(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "team")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON, "slow.ipynb", nbJSON)
	base := "/v1/spaces/" + sp.ID
	status := func(method, path, user, body string) int {
		t.Helper()
		if body != "" {
			return e.req(method, path, user, strings.NewReader(body), "application/json").Code
		}
		return e.req(method, path, user, nil, "").Code
	}
	grant := func(who, role string) {
		t.Helper()
		if c := status("PUT", base+"/members/"+who, "alice", `{"role":"`+role+`"}`); c != http.StatusOK {
			t.Fatalf("grant %s %s: %d", who, role, c)
		}
	}
	aliceRun := e.runToEnd(t, "alice", sp.ID, `{"notebook":"nb.ipynb"}`)

	// No role: everything is 404, as for a missing space.
	for _, c := range []struct{ method, path, body string }{
		{"GET", base, ""}, {"GET", base + "/members", ""}, {"GET", base + "/runs", ""},
		{"POST", base + "/runs", `{"notebook":"nb.ipynb"}`}, {"PUT", base + "/members/bob", `{"role":"owner"}`},
	} {
		if got := status(c.method, c.path, "bob", c.body); got != http.StatusNotFound {
			t.Errorf("bob without a role: %s %s = %d, want 404", c.method, c.path, got)
		}
	}

	// read: sees the space, its files, members and every run; changes nothing.
	grant("bob", "read")
	var got spaceJSON
	_ = json.Unmarshal(e.req("GET", base, "bob", nil, "").Body.Bytes(), &got)
	if got.Role != "read" || got.Owner != "alice" {
		t.Fatalf("bob's view of the space: %+v", got)
	}
	var list struct{ Items []spaceJSON }
	_ = json.Unmarshal(e.req("GET", "/v1/spaces?role=read", "bob", nil, "").Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0].ID != sp.ID {
		t.Fatalf("bob's shared spaces: %+v", list)
	}
	var runList struct{ Items []runs.View }
	_ = json.Unmarshal(e.req("GET", base+"/runs", "bob", nil, "").Body.Bytes(), &runList)
	if len(runList.Items) != 1 || runList.Items[0].Owner != "alice" {
		t.Fatalf("bob sees alice's runs: %+v", runList.Items)
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", base + "/files", "", http.StatusOK},
		{"GET", base + "/files/nb.ipynb", "", http.StatusOK},
		{"GET", base + "/members", "", http.StatusOK},
		{"GET", base + "/runs/" + aliceRun.ID + "/output.html", "", http.StatusOK},
		{"POST", base + "/runs", `{"notebook":"nb.ipynb"}`, http.StatusForbidden},
		{"POST", base + "/sessions", `{"notebook":"nb.ipynb"}`, http.StatusForbidden},
		{"PUT", base + "/members/carol", `{"role":"read"}`, http.StatusForbidden},
		{"PATCH", base, `{"name":"bob's"}`, http.StatusForbidden},
		{"DELETE", base, "", http.StatusForbidden},
		{"POST", base + "/folders", `{"path":"bobs"}`, http.StatusForbidden},
	} {
		if got := status(c.method, c.path, "bob", c.body); got != c.want {
			t.Errorf("bob with read: %s %s = %d, want %d", c.method, c.path, got, c.want)
		}
	}

	// write: runs and sessions of their own; others' runs only to look at.
	grant("bob", "write")
	bobRun := e.runToEnd(t, "bob", sp.ID, `{"notebook":"nb.ipynb"}`)
	if bobRun.Status != runs.StatusSucceeded || bobRun.Owner != "bob" {
		t.Fatalf("bob's run: %+v", bobRun)
	}
	var mine struct{ Items []runs.View }
	_ = json.Unmarshal(e.req("GET", base+"/runs?owner=bob", "alice", nil, "").Body.Bytes(), &mine)
	if len(mine.Items) != 1 || mine.Items[0].ID != bobRun.ID {
		t.Fatalf("runs by bob: %+v", mine.Items)
	}
	if c := status("DELETE", base+"/runs/"+aliceRun.ID, "bob", ""); c != http.StatusForbidden {
		t.Errorf("bob deletes alice's run: %d", c)
	}
	if c := status("DELETE", base+"/runs/"+bobRun.ID, "alice", ""); c != http.StatusNoContent {
		t.Errorf("alice (an owner) deletes bob's run: %d", c)
	}
	if c := status("POST", base+"/folders", "bob", `{"path":"bobs"}`); c != http.StatusCreated {
		t.Errorf("bob with write creates a folder: %d", c)
	}
	// Sessions are their opener's: the same notebook gives each their own.
	var aliceSess, bobSess struct{ ID string }
	_ = json.Unmarshal(e.req("POST", base+"/sessions", "alice", strings.NewReader(`{"notebook":"nb.ipynb","mode":"read-only"}`), "application/json").Body.Bytes(), &aliceSess)
	_ = json.Unmarshal(e.req("POST", base+"/sessions", "bob", strings.NewReader(`{"notebook":"nb.ipynb","mode":"read-only"}`), "application/json").Body.Bytes(), &bobSess)
	if aliceSess.ID == "" || bobSess.ID == "" || aliceSess.ID == bobSess.ID {
		t.Fatalf("sessions: alice %q, bob %q", aliceSess.ID, bobSess.ID)
	}
	if c := status("GET", base+"/sessions/"+aliceSess.ID, "bob", ""); c != http.StatusNotFound {
		t.Errorf("bob reads alice's session: %d", c)
	}
	var sessList struct{ Items []json.RawMessage }
	_ = json.Unmarshal(e.req("GET", base+"/sessions", "bob", nil, "").Body.Bytes(), &sessList)
	if len(sessList.Items) != 1 {
		t.Errorf("bob lists %d sessions, want his own", len(sessList.Items))
	}
	_ = json.Unmarshal(e.req("GET", base+"/sessions", "alice", nil, "").Body.Bytes(), &sessList)
	if len(sessList.Items) != 2 {
		t.Errorf("alice (an owner) lists %d sessions, want both", len(sessList.Items))
	}
	if c := status("DELETE", base+"/sessions/"+bobSess.ID, "alice", ""); c != http.StatusOK && c != http.StatusNoContent {
		t.Errorf("alice (an owner) ends bob's session: %d", c)
	}

	// Members: listed with the creator first; leaving; handing over.
	var members struct{ Items []memberJSON }
	_ = json.Unmarshal(e.req("GET", base+"/members", "bob", nil, "").Body.Bytes(), &members)
	if len(members.Items) != 2 || !members.Items[0].Creator || members.Items[1].Principal != "bob" || members.Items[1].Role != "write" {
		t.Fatalf("members: %+v", members.Items)
	}
	if c := status("PUT", base+"/members/bob", "alice", `{"role":"admin"}`); c != http.StatusBadRequest {
		t.Errorf("an unknown role: %d", c)
	}
	grant("bob", "owner")
	if c := status("POST", base+"/transfer", "alice", `{"to":"bob"}`); c != http.StatusOK {
		t.Fatalf("hand over: %d", c)
	}
	_ = json.Unmarshal(e.req("GET", base, "alice", nil, "").Body.Bytes(), &got)
	if got.Owner != "bob" || got.Role != "owner" {
		t.Fatalf("after handing over: %+v", got)
	}
	if c := status("DELETE", base+"/members/alice", "alice", ""); c != http.StatusNoContent {
		t.Fatalf("alice leaves: %d", c)
	}
	if c := status("GET", base, "alice", ""); c != http.StatusNotFound {
		t.Errorf("after leaving: %d", c)
	}
	// Grants and revocations are in the audit log.
	page := e.audit(t, "?space_id="+sp.ID)
	ops := map[string]int{}
	for _, it := range page.Items {
		ops[it.Action]++
	}
	if ops["PutMember"] != 6 || ops["TransferSpace"] != 1 || ops["DeleteMember"] != 1 { // 3 grants and 3 refusals
		t.Errorf("audited: %v", ops)
	}
}
