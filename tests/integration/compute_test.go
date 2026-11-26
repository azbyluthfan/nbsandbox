// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type workerJSON struct {
	ID, State, Role, Runtime string
	Runs, Sessions           struct{ Active, Capacity int }
}

func workers(t *testing.T, user string) []workerJSON {
	t.Helper()
	r := call(t, "GET", "/v1/workers", user, nil, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("workers: %d %s", r.Code, r.Body)
	}
	var list struct{ Items []workerJSON }
	r.json(t, &list)
	return list.Items
}

// The compute view (docs/plan-compute-view.md): every worker is listed, a
// run counts on the worker that holds it and names it, and only
// administrators see whose runs are where.
func TestComputeWorkers(t *testing.T) {
	var live []workerJSON
	for _, w := range workers(t, "bob") { // any signed-in user
		if w.State == "up" {
			live = append(live, w)
		}
	}
	want, role := 1, "all"
	if clustered || onKubernetes {
		want, role = 2, "worker"
	}
	listed := len(live) == want
	for _, w := range live {
		listed = listed && w.Role == role && w.Runs.Capacity > 0 && w.Runtime != ""
	}

	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"long.ipynb": ""})
	run := startRun(t, "alice", sp, "long.ipynb", nil)
	waitLog(t, "alice", sp, run.ID, "tick 2", 3*time.Minute)
	held := getRun(t, "alice", sp, run.ID).Worker
	counted := false
	for _, w := range workers(t, "bob") {
		counted = counted || w.ID == held && w.Runs.Active >= 1
	}
	refused := call(t, "GET", "/v1/admin/runs?worker="+held, "alice", nil, nil).Code == http.StatusForbidden
	call(t, "POST", "/v1/spaces/"+sp+"/runs/"+run.ID+"/cancel", "alice", nil, nil)
	waitRun(t, "alice", sp, run.ID, time.Minute)
	keeps := getRun(t, "alice", sp, run.ID).Worker == held

	got := fmt.Sprintf("%d up (want %d, role %s): %v; the run's worker %q counted: %v; still named once it ended: %v; the admin filter refused to a user: %v",
		len(live), want, role, ids(live), held, counted, keeps, refused)
	ok := listed && held != "" && counted && keeps && refused
	finding(t, "compute-workers", "list the workers as a user, run a notebook, look again",
		"every worker listed with its role and capacity; the run counts on the worker that holds it, which it names, also once it ended; only administrators filter runs by worker", got, ok)
	if !ok {
		t.Error(got)
	}
}

func ids(ws []workerJSON) string {
	var out []string
	for _, w := range ws {
		out = append(out, w.ID)
	}
	return strings.Join(out, ", ")
}
