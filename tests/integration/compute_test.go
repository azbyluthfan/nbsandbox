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
	Usage                    *struct {
		MemoryBytes         int64 `json:"memory_bytes"`
		MemoryReservedBytes int64 `json:"memory_reserved_bytes"`
	}
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

// A run's usage over its life (milestone 2): its worker samples it every few
// seconds; the samples follow what the notebook does, its worker's totals
// show it while it runs, and they stay once it ends. On Kubernetes the
// samples come from metrics-server, every 15 s and later after the start:
// the notebook runs longer there.
func TestComputeRunUsage(t *testing.T) {
	steps, minSamples := 45, 3
	if onKubernetes {
		steps, minSamples = 110, 2
	}
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"grow.ipynb": codeNotebook(fmt.Sprintf(`import time
blocks = []
for i in range(%d):
    blocks.append(bytearray(6 << 20))
    sum(j * j for j in range(200000))
    time.sleep(0.5)
print("grown", flush=True)
time.sleep(5)`, steps))})
	run := startRun(t, "alice", sp, "grow.ipynb", nil)
	waitLog(t, "alice", sp, run.ID, "Executing Cell", 3*time.Minute)
	time.Sleep(12*time.Second + time.Duration(steps-45)/2*time.Second) // samples taken while it grows
	held := getRun(t, "alice", sp, run.ID).Worker
	var sampled bool
	for _, w := range workers(t, "bob") {
		sampled = sampled || w.ID == held && w.Usage != nil && w.Usage.MemoryBytes > 0 && w.Usage.MemoryReservedBytes > 0
	}
	waitRun(t, "alice", sp, run.ID, 3*time.Minute)

	type usage struct {
		Limits struct {
			MemoryBytes int64 `json:"memory_bytes"`
		}
		Samples []struct {
			MemoryBytes int64   `json:"memory_bytes"`
			CPUs        float64 `json:"cpus"`
		}
	}
	r := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/usage", "alice", nil, nil)
	var u usage
	r.json(t, &u)
	n := len(u.Samples)
	grew := n >= minSamples && u.Samples[n-1].MemoryBytes > u.Samples[0].MemoryBytes+100<<20 && u.Samples[n-1].MemoryBytes < u.Limits.MemoryBytes
	busy := false
	for _, s := range u.Samples {
		busy = busy || s.CPUs > 0.01
	}
	other := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/usage", "bob", nil, nil).Code

	got := fmt.Sprintf("%d samples, memory %d → %d MiB (limit %d MiB), CPU seen: %v; its worker's totals while it ran: %v; another user: %d",
		n, first(u.Samples).MemoryBytes>>20, last(u.Samples).MemoryBytes>>20, u.Limits.MemoryBytes>>20, busy, sampled, other)
	ok := grew && busy && sampled && other == http.StatusNotFound
	finding(t, "compute-run-usage", "a notebook that grows by 6 MiB every half second and computes; read its usage during and after",
		"samples every few seconds follow its memory up to below its limit and show its CPU; its worker's totals include it while it runs; another user gets 404", got, ok)
	if !ok {
		t.Error(got)
	}
}

func first[T any](s []T) T {
	var z T
	if len(s) == 0 {
		return z
	}
	return s[0]
}

func last[T any](s []T) T {
	var z T
	if len(s) == 0 {
		return z
	}
	return s[len(s)-1]
}
