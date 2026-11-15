// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/runs"
)

type auditPage struct {
	Items      []auditJSON `json:"items"`
	NextCursor string      `json:"next_cursor"`
}

func (e testEnv) audit(t *testing.T, query string) auditPage {
	t.Helper()
	rec := e.req("GET", "/v1/admin/audit"+query, "root", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit%s: %d %s", query, rec.Code, rec.Body)
	}
	var p auditPage
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return p
}

// find returns the newest row with this action and actor.
func (p auditPage) find(action, actor string) (auditJSON, bool) {
	for _, e := range p.Items {
		if e.Action == action && e.Actor == actor {
			return e, true
		}
	}
	return auditJSON{}, false
}

// Changes, downloads and refusals are recorded with who, what and how they
// ended; listings are not.
func TestAuditRecordsOperations(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "audited")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON)
	v := e.runToEnd(t, "alice", sp.ID, `{"notebook":"nb.ipynb"}`)
	e.req("GET", "/v1/spaces/"+sp.ID+"/files/nb.ipynb", "alice", nil, "")
	e.req("GET", "/v1/spaces/"+sp.ID+"/files", "alice", nil, "") // a listing: not recorded
	if rec := e.req("GET", "/v1/spaces/"+sp.ID+"/files/nb.ipynb", "bob", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob: %d", rec.Code)
	}
	e.req("PATCH", "/v1/spaces/"+sp.ID+"/files/nb.ipynb", "alice", strings.NewReader(`{"path":"moved.ipynb"}`), "application/json")

	log := e.audit(t, "?limit=100")
	for _, want := range []struct {
		action, actor, target, detail string
		status                        int
	}{
		{"CreateSpace", "alice", "", "name audited", 201},
		{"UploadFiles", "alice", "", "nb.ipynb", 201},
		{"CreateRun", "alice", v.ID, "notebook nb.ipynb", 201},
		{"GetFile", "bob", "nb.ipynb", "", 404},
		{"MoveFile", "alice", "nb.ipynb", "to moved.ipynb", 200},
	} {
		got, ok := log.find(want.action, want.actor)
		if !ok {
			t.Errorf("%s by %s not recorded", want.action, want.actor)
			continue
		}
		if got.SpaceID != sp.ID || got.Target != want.target || got.Detail != want.detail || got.Status != want.status ||
			got.Via != "rest" || got.Method != "basic" || got.Address == "" {
			t.Errorf("%s by %s: %+v", want.action, want.actor, got)
		}
	}
	if _, ok := log.find("GetFile", "alice"); !ok {
		t.Error("a download is recorded")
	}
	if _, ok := log.find("ListFiles", "alice"); ok {
		t.Error("a listing was recorded")
	}

	// Reading the audit log needs the admin scope, and both the refusal and
	// the reading are recorded.
	if rec := e.req("GET", "/v1/admin/audit", "alice", nil, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("alice reads the audit log: %d", rec.Code)
	}
	log = e.audit(t, "?action=AdminListAudit")
	if got, ok := log.find("AdminListAudit", "alice"); !ok || got.Status != 403 {
		t.Errorf("alice's refused read: %+v", got)
	}
	if _, ok := log.find("AdminListAudit", "root"); !ok {
		t.Error("root's read")
	}

	// Filters and paging.
	if p := e.audit(t, "?actor=bob"); len(p.Items) != 1 || p.Items[0].Action != "GetFile" {
		t.Errorf("bob's: %+v", p.Items)
	}
	first := e.audit(t, "?space="+sp.ID+"&limit=2")
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("page: %+v", first)
	}
	id := func(s string) int { n, _ := strconv.Atoi(s); return n }
	if next := e.audit(t, "?space="+sp.ID+"&limit=2&cursor="+first.NextCursor); len(next.Items) == 0 || id(next.Items[0].ID) >= id(first.Items[1].ID) {
		t.Errorf("next page doesn't continue: %+v", next.Items)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if p := e.audit(t, "?since="+future); len(p.Items) != 0 {
		t.Errorf("since the future: %d rows", len(p.Items))
	}
	for _, q := range []string{"?since=yesterday", "?cursor=x"} {
		if rec := e.req("GET", "/v1/admin/audit"+q, "root", nil, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}

func TestAdminRunsCancelAndDrain(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON, "slow.ipynb", nbJSON)
	slow := e.startRun(t, sp.ID, "slow.ipynb")

	for _, r := range [][2]string{{"GET", "/v1/admin/runs"}, {"POST", "/v1/admin/runs/" + slow.ID + "/cancel"}, {"GET", "/v1/admin/drain"}, {"PUT", "/v1/admin/drain"}} {
		if rec := e.req(r[0], r[1], "alice", strings.NewReader(`{"draining":true}`), "application/json"); rec.Code != http.StatusForbidden {
			t.Errorf("alice %s %s: %d", r[0], r[1], rec.Code)
		}
	}
	var list struct{ Items []runs.AdminView }
	rec := e.req("GET", "/v1/admin/runs?owner=alice&status=running", "root", nil, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	deadline := time.Now().Add(5 * time.Second)
	for len(list.Items) == 0 && time.Now().Before(deadline) { // queued until a worker takes it
		time.Sleep(10 * time.Millisecond)
		rec = e.req("GET", "/v1/admin/runs?owner=alice&status=running", "root", nil, "")
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
	}
	if rec.Code != http.StatusOK || len(list.Items) != 1 || list.Items[0].ID != slow.ID || list.Items[0].Owner != "alice" || list.Items[0].Result != nil {
		t.Fatalf("running runs: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/v1/admin/runs?space=nope", "root", nil, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad space: %d", rec.Code)
	}

	// Stop alice's run.
	rec = e.req("POST", "/v1/admin/runs/"+slow.ID+"/cancel", "root", strings.NewReader(`{"reason":"runaway"}`), "application/json")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	done := e.waitRun(t, "alice", sp.ID, slow.ID)
	if done.Status != runs.StatusCancelled || done.CancelledBy != "root" || done.CancelReason != "runaway" {
		t.Fatalf("after the admin's cancel: %+v", done)
	}
	if rec := e.req("POST", "/v1/admin/runs/"+slow.ID+"/cancel", "root", nil, ""); rec.Code != http.StatusConflict {
		t.Errorf("cancel a finished run: %d", rec.Code)
	}
	if rec := e.req("POST", "/v1/admin/runs/00000000-0000-4000-8000-000000000000/cancel", "root", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("cancel an unknown run: %d", rec.Code)
	}
	var cancelled *auditJSON
	for _, r := range e.audit(t, "?action=AdminCancelRun").Items {
		if r.Status == http.StatusAccepted {
			cancelled = &r
		}
	}
	if cancelled == nil || cancelled.SpaceID != sp.ID || cancelled.Target != slow.ID || !strings.Contains(cancelled.Detail, "owner alice") {
		t.Errorf("cancel's audit row: %+v", cancelled)
	}

	// Drain: a new run waits, queued, until resumed.
	rec = e.req("PUT", "/v1/admin/drain", "root", strings.NewReader(`{"draining":true,"reason":"upgrade"}`), "application/json")
	var d drainJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if rec.Code != http.StatusOK || !d.Draining || d.Reason != "upgrade" || d.By != "root" || d.Since == nil {
		t.Fatalf("drain: %d %s", rec.Code, rec.Body)
	}
	queued := e.startRun(t, sp.ID, "nb.ipynb")
	time.Sleep(300 * time.Millisecond)
	rec = e.req("GET", "/v1/admin/drain", "root", nil, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Queued != 1 || d.Running != 0 {
		t.Fatalf("while drained: %s", rec.Body)
	}
	if rec := e.req("PUT", "/v1/admin/drain", "root", strings.NewReader(`{}`), "application/json"); rec.Code != http.StatusBadRequest {
		t.Errorf("no draining: %d", rec.Code)
	}
	rec = e.req("PUT", "/v1/admin/drain", "root", strings.NewReader(`{"draining":false}`), "application/json")
	d = drainJSON{}
	if _ = json.Unmarshal(rec.Body.Bytes(), &d); rec.Code != http.StatusOK || d.Draining || d.Since != nil {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	if v := e.waitRun(t, "alice", sp.ID, queued.ID); v.Status != runs.StatusSucceeded {
		t.Fatalf("after resuming: %+v", v)
	}
}
