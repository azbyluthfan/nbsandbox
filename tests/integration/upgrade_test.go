// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The upgrade test (make test-upgrade, scripts/upgrade-test.sh). Phase
// "before" runs against an earlier release and makes what an operator's
// users would have: spaces, files, finished runs, a published function, an
// open session holding a variable, runs active and queued when nbsandboxd
// stops. Phase "after" runs against this checkout on the same database and
// object store: the migrations applied on top of that data, and everything
// made before is still there and still works. Skipped unless
// NBSANDBOX_UPGRADE is set.

type upgradeState struct {
	Kept, Failed, Session string // spaces
	OK, Fail              string // finished runs in Kept and Failed
	OKResult              json.RawMessage
	FnResult              json.RawMessage
	SessionID             string
	Active                []upgradeRun // active when nbsandboxd stopped
	BobSpace              string
	Data                  string
	Schema                int // the database's schema version before the upgrade
}

type upgradeRun struct {
	Space, ID, Status string
	Long              bool
}

// schemaVersions lists the migrations the upgrade stack's database has applied.
func schemaVersions(t *testing.T) []int {
	t.Helper()
	q := []string{"psql", "-U", "nbsandbox", "-d", "nbsandbox", "-tAc", "SELECT version FROM schema_version ORDER BY version"}
	var out string
	if onKubernetes { // make test-helm-upgrade: the kind stack's PostgreSQL
		out = kubectl(t, append([]string{"-n", "nbsandbox", "exec", "deploy/postgres", "--"}, q...)...)
	} else {
		out = docker(t, append([]string{"exec", "nbsandbox-upgrade-postgres-1"}, q...)...)
	}
	var v []int
	for _, f := range strings.Fields(out) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("schema_version: %q", out)
		}
		v = append(v, n)
	}
	return v
}

// latestMigration is the highest migration this checkout has.
func latestMigration(t *testing.T) int {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(repoRoot(), "internal", "store", "migrations", "*.sql"))
	latest := 0
	for _, f := range files {
		n, _ := strconv.Atoi(strings.SplitN(filepath.Base(f), "_", 2)[0])
		latest = max(latest, n)
	}
	return latest
}

const upgradeData = "region,n\neu,3\nus,4\neu,5\n"

// sameJSON compares two JSON documents, whatever their key order.
func sameJSON(a, b []byte) bool {
	norm := func(b []byte) string {
		var v any
		if json.Unmarshal(b, &v) != nil {
			return string(b)
		}
		out, _ := json.Marshal(v)
		return string(out)
	}
	return norm(a) == norm(b)
}

func TestUpgrade(t *testing.T) {
	phase, path := os.Getenv("NBSANDBOX_UPGRADE"), os.Getenv("NBSANDBOX_UPGRADE_STATE")
	if phase == "" || path == "" {
		t.Skip("make test-upgrade")
	}
	switch phase {
	case "before":
		st := upgradeBefore(t)
		b, _ := json.MarshalIndent(st, "", "  ")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	case "after":
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var st upgradeState
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		upgradeAfter(t, st)
	default:
		t.Fatalf("NBSANDBOX_UPGRADE=%q: before or after", phase)
	}
}

// keptSpace creates a space that outlives the test (phase after deletes it).
func keptSpace(t *testing.T, user, name string) string {
	t.Helper()
	r := jsonCall(t, "POST", "/v1/spaces", user, map[string]string{"name": name}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("create space: %d %s", r.Code, r.Body)
	}
	var sp struct{ ID string }
	r.json(t, &sp)
	return sp.ID
}

var (
	upgradeOK = codeNotebook(`import csv, nbsandbox
rows = list(csv.DictReader(open("data/in.csv")))
nbsandbox.output({"rows": len(rows), "total": sum(int(r["n"]) for r in rows)})`)
	upgradeLong  = codeNotebook("import time\ntime.sleep(900)")
	upgradeQuick = codeNotebook("import nbsandbox\nnbsandbox.output('after')")
)

func upgradeBefore(t *testing.T) upgradeState {
	st := upgradeState{Data: upgradeData}
	v := schemaVersions(t)
	if len(v) == 0 || v[len(v)-1] >= latestMigration(t) {
		t.Fatalf("the earlier release's schema is %v; this checkout's latest migration is %d: nothing to upgrade", v, latestMigration(t))
	}
	st.Schema = v[len(v)-1]
	st.Kept = keptSpace(t, "alice", "upgrade-kept")
	upload(t, "alice", st.Kept, "", map[string]string{"ok.ipynb": upgradeOK})
	upload(t, "alice", st.Kept, "data", map[string]string{"in.csv": upgradeData})
	ok := startRun(t, "alice", st.Kept, "ok.ipynb", nil)
	res := runResult(t, "alice", st.Kept, ok.ID)
	if res.Status != "succeeded" {
		t.Fatalf("ok run: %s %s", res.Status, res.Output)
	}
	st.OK, st.OKResult = ok.ID, res.Output

	st.Failed = keptSpace(t, "alice", "upgrade-failed")
	upload(t, "alice", st.Failed, "", map[string]string{"fail.ipynb": codeNotebook("raise ValueError('planned failure')")})
	fail := startRun(t, "alice", st.Failed, "fail.ipynb", nil)
	if got := waitRun(t, "alice", st.Failed, fail.ID, 3*time.Minute); got.Status != "failed" {
		t.Fatalf("fail run: %s %s", got.Status, got.Message)
	}
	st.Fail = fail.ID

	r := jsonCall(t, "POST", "/v1/spaces/"+st.Kept+"/functions/totals/versions", "alice", map[string]any{"notebook": "ok.ipynb", "files": []string{"data"}}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", r.Code, r.Body)
	}
	code, fres, body := invoke(t, st.Kept, "totals/invoke", map[string]any{})
	if code != http.StatusOK || fres.Status != "succeeded" {
		t.Fatalf("invoke: %d %s", code, body)
	}
	st.FnResult = fres.Output

	st.Session = keptSpace(t, "alice", "upgrade-session")
	upload(t, "alice", st.Session, "", map[string]string{"nb.ipynb": codeNotebook("1")})
	r = jsonCall(t, "POST", "/v1/spaces/"+st.Session+"/sessions", "alice", map[string]string{"notebook": "nb.ipynb"}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("open session: %d %s", r.Code, r.Body)
	}
	var s sessionJSON
	r.json(t, &s)
	st.SessionID = s.ID
	k := dialSession(t, "alice", st.Session, s.ID)
	k.run("kept = 41 + 1")
	_ = k.c.Close()

	st.BobSpace = keptSpace(t, "bob", "upgrade-bob")

	// Runs active when nbsandboxd stops: two long ones take the run slots
	// (2 in the dev stack, 2 workers of one on kind), a quick one waits
	// behind them.
	for i, nb := range []string{"long", "long", "quick"} {
		sp := keptSpace(t, "alice", fmt.Sprintf("upgrade-active-%d", i))
		content := upgradeLong
		if nb == "quick" {
			content = upgradeQuick
		}
		upload(t, "alice", sp, "", map[string]string{"nb.ipynb": content})
		run := startRun(t, "alice", sp, "nb.ipynb", nil)
		st.Active = append(st.Active, upgradeRun{Space: sp, ID: run.ID, Long: nb == "long"})
		if nb == "long" {
			deadline := time.Now().Add(2 * time.Minute)
			for getRun(t, "alice", sp, run.ID).Status != "running" {
				if time.Now().After(deadline) {
					t.Fatalf("long run %s didn't start", run.ID)
				}
				time.Sleep(time.Second)
			}
		}
	}
	for i := range st.Active {
		st.Active[i].Status = getRun(t, "alice", st.Active[i].Space, st.Active[i].ID).Status
	}
	t.Logf("before the upgrade: %+v", st.Active)
	return st
}

func upgradeAfter(t *testing.T, st upgradeState) {
	var problems []string
	check := func(ok bool, format string, args ...any) {
		t.Helper()
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	for _, sp := range []string{st.Kept, st.Failed, st.Session} {
		t.Cleanup(func() { call(t, "DELETE", "/v1/spaces/"+sp, "alice", nil, nil) })
	}
	for _, a := range st.Active {
		t.Cleanup(func() { call(t, "DELETE", "/v1/spaces/"+a.Space, "alice", nil, nil) })
	}
	t.Cleanup(func() { call(t, "DELETE", "/v1/spaces/"+st.BobSpace, "bob", nil, nil) })
	t.Cleanup(func() {
		call(t, "DELETE", "/v1/spaces/"+st.Session+"/sessions/"+st.SessionID, "alice", nil, nil)
		assertNoSessionContainer(t, st.SessionID)
	})

	// Every migration since the earlier release applied, once each.
	v := schemaVersions(t)
	latest := latestMigration(t)
	check(len(v) == latest && v[0] == 1 && v[len(v)-1] == latest, "schema versions after the upgrade: %v (was up to %d, latest %d)", v, st.Schema, latest)

	// Spaces and their owners.
	var mine struct{ Items []struct{ ID string } }
	call(t, "GET", "/v1/spaces", "alice", nil, nil).json(t, &mine)
	ids := map[string]bool{}
	for _, s := range mine.Items {
		ids[s.ID] = true
	}
	check(ids[st.Kept] && ids[st.Failed] && ids[st.Session] && !ids[st.BobSpace], "alice's spaces after the upgrade: %v", ids)
	check(call(t, "GET", "/v1/spaces/"+st.BobSpace, "bob", nil, nil).Code == http.StatusOK, "bob's space is gone")
	check(call(t, "GET", "/v1/spaces/"+st.BobSpace, "alice", nil, nil).Code == http.StatusNotFound, "alice sees bob's space")

	// Files.
	if r := call(t, "GET", "/v1/spaces/"+st.Kept+"/files/data/in.csv", "alice", nil, nil); r.Code != http.StatusOK || string(r.Body) != st.Data {
		problems = append(problems, fmt.Sprintf("data/in.csv: %d %q", r.Code, r.Body))
	}

	// Finished runs: status, result, log, output notebook, events.
	check(getRun(t, "alice", st.Kept, st.OK).Status == "succeeded", "the finished run's status changed")
	res := runResult(t, "alice", st.Kept, st.OK)
	check(sameJSON(res.Output, st.OKResult), "the finished run's result: %s, was %s", res.Output, st.OKResult)
	for _, p := range []string{"/log", "/output.ipynb", "/events"} {
		r := call(t, "GET", "/v1/spaces/"+st.Kept+"/runs/"+st.OK+p, "alice", nil, nil)
		check(r.Code == http.StatusOK && len(r.Body) > 0, "the finished run's %s: %d %.200s", p, r.Code, r.Body)
	}
	failed := getRun(t, "alice", st.Failed, st.Fail)
	check(failed.Status == "failed" && strings.Contains(failed.Message, "planned failure"), "the failed run: %s %q", failed.Status, failed.Message)

	// The same notebook runs again, with the same result.
	again := runResult(t, "alice", st.Kept, startRun(t, "alice", st.Kept, "ok.ipynb", nil).ID)
	check(again.Status == "succeeded" && sameJSON(again.Output, st.OKResult), "a new run: %s %s", again.Status, again.Output)

	// The function published before: version 1, same answer.
	code, fres, body := invoke(t, st.Kept, "totals/invoke", map[string]any{})
	check(code == http.StatusOK && fres.Status == "succeeded" && fres.Version == 1 && sameJSON(fres.Output, st.FnResult),
		"the function published before: %d %s", code, body)

	// The open session is re-attached, with its variable.
	k := connectSession(t, "alice", st.Session, st.SessionID)
	out, _ := k.run("print(kept)")
	check(strings.TrimSpace(out) == "42", "the session's variable after the upgrade: %q", out)

	// Runs active at the stop: running ones end interrupted, queued ones
	// start after it.
	for _, a := range st.Active {
		switch a.Status {
		case "running":
			got := waitRun(t, "alice", a.Space, a.ID, 2*time.Minute)
			check(got.Status == "interrupted", "run %s, running at the stop: %s %q", a.ID, got.Status, got.Message)
		case "queued":
			if a.Long {
				deadline := time.Now().Add(2 * time.Minute)
				for getRun(t, "alice", a.Space, a.ID).Status == "queued" && time.Now().Before(deadline) {
					time.Sleep(time.Second)
				}
				got := getRun(t, "alice", a.Space, a.ID)
				check(got.Status == "running", "run %s, queued at the stop: %s", a.ID, got.Status)
				call(t, "POST", "/v1/spaces/"+a.Space+"/runs/"+a.ID+"/cancel", "alice", nil, nil)
				waitRun(t, "alice", a.Space, a.ID, time.Minute)
				continue
			}
			got := runResult(t, "alice", a.Space, a.ID)
			check(got.Status == "succeeded", "run %s, queued at the stop: %s %s", a.ID, got.Status, got.Output)
		default:
			problems = append(problems, fmt.Sprintf("run %s was %s before the stop", a.ID, a.Status))
		}
	}

	// What the new migrations added works on the old spaces: a connection,
	// bound to one and unbound again.
	r := jsonCall(t, "POST", "/v1/connections", "alice", map[string]any{"name": "upgrade-crm", "type": "postgresql",
		"settings": map[string]any{"host": upgradeConnHost(), "database": "crm", "user": "reader"}, "secret": map[string]any{"value": "x-upgrade"}}, nil)
	if r.Code != http.StatusCreated {
		problems = append(problems, fmt.Sprintf("create a connection: %d %s", r.Code, r.Body))
	} else {
		var c struct{ ID string }
		r.json(t, &c)
		t.Cleanup(func() { call(t, "DELETE", "/v1/connections/"+c.ID, "alice", nil, nil) })
		r = jsonCall(t, "PUT", "/v1/spaces/"+st.Kept+"/connections", "alice", map[string]any{"default_environment": "main",
			"environments": map[string]map[string]string{"main": {"crm": c.ID}}}, nil)
		check(r.Code == http.StatusOK, "bind on a space made before: %d %s", r.Code, r.Body)
		r = call(t, "GET", "/v1/spaces/"+st.Kept+"/connections", "alice", nil, nil)
		check(r.Code == http.StatusOK && bytes.Contains(r.Body, []byte(c.ID)), "the space's bindings: %d %s", r.Code, r.Body)
		r = jsonCall(t, "PUT", "/v1/spaces/"+st.Kept+"/connections", "alice", map[string]any{"environments": map[string]map[string]string{}}, nil)
		check(r.Code == http.StatusOK, "unbind: %d %s", r.Code, r.Body)
	}

	got := fmt.Sprintf("schema %d → %d; everything made before is there and works", st.Schema, latest)
	if len(problems) > 0 {
		got = strings.Join(problems, "; ")
	}
	finding(t, "upgrade", "upgrade from "+os.Getenv("FROM")+" on the same database and object store, with a session open and runs active",
		"files, runs, results, a published function and the session's kernel survive; running runs end interrupted, queued ones start; the new tables work on old spaces", got, len(problems) == 0)
	if len(problems) > 0 {
		t.Error(got)
	}
	assertNoRunContainers(t)
}

// upgradeConnHost is a host connections may point to: what each stack's
// connections.allowed lists.
func upgradeConnHost() string {
	if onKubernetes {
		return "nbsandbox-src-pg"
	}
	return "db.example.com"
}
