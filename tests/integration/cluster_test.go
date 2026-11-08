// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Failure tests for the cluster stack (make test-cluster): two API
// processes behind a load balancer, two workers, each running one notebook
// at a time, with 6 s leases. They stop or kill single containers.

// longStream emits one item a second, n of them.
const longStream = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {"kernelspec": {"name": "python3", "display_name": "Python 3", "language": "python"}},
 "cells": [
  {"cell_type": "code", "metadata": {"tags": ["parameters"]}, "execution_count": null, "outputs": [], "source": "n = 20"},
  {"cell_type": "code", "metadata": {}, "execution_count": null, "outputs": [],
   "source": "import time, nbsandbox\nfor i in range(n):\n    nbsandbox.emit({'i': i}, total=n)\n    time.sleep(1)"}
 ]}`

func needCluster(t *testing.T) {
	t.Helper()
	if !clustered || onKubernetes {
		t.Skip("cluster stack only (make test-cluster)")
	}
}

// container names one replica of a cluster service.
func container(service string, n int) string {
	return fmt.Sprintf("nbsandbox-cluster-%s-%d", service, n)
}

// clusterResult is GET .../result's answer.
type clusterResult struct {
	RunID   string            `json:"run_id"`
	Status  string            `json:"status"`
	Output  []json.RawMessage `json:"output"`
	Items   *int              `json:"items"`
	Partial bool              `json:"partial"`
	Error   *struct {
		Kind      string `json:"kind"`
		Fault     string `json:"fault"`
		Retryable bool   `json:"retryable"`
		Message   string `json:"message"`
	} `json:"error"`
}

// resultFollower reads a run's result stream like a careful client: it
// keeps every item once, reconnects after a dropped connection with
// Last-Event-ID, and stops at the result.
type resultFollower struct {
	mu         sync.Mutex
	items      []int
	reconnects int
	result     *clusterResult
	err        error
	done       chan struct{}
}

func followResult(t *testing.T, sp, runID string) *resultFollower {
	t.Helper()
	f := &resultFollower{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			req, _ := http.NewRequest("GET", baseURL+"/v1/spaces/"+sp+"/runs/"+runID+"/result", nil)
			req.SetBasicAuth("alice", passwords["alice"])
			req.Header.Set("Accept", "text/event-stream")
			f.mu.Lock()
			if n := len(f.items); n > 0 {
				req.Header.Set("Last-Event-ID", strconv.Itoa(f.items[n-1]))
			}
			f.mu.Unlock()
			resp, err := streamClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				if f.read(resp) {
					return
				}
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			f.mu.Lock()
			f.reconnects++
			f.mu.Unlock()
			time.Sleep(500 * time.Millisecond)
		}
		f.err = errors.New("no result within 3 minutes")
	}()
	return f
}

// read consumes one connection; true once the result arrived.
func (f *resultFollower) read(resp *http.Response) bool {
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			switch event {
			case "item":
				var it struct {
					Index int `json:"index"`
				}
				_ = json.Unmarshal([]byte(data), &it)
				f.mu.Lock()
				f.items = append(f.items, it.Index) // duplicates would show here
				f.mu.Unlock()
			case "result":
				var r clusterResult
				_ = json.Unmarshal([]byte(data), &r)
				f.mu.Lock()
				f.result = &r
				f.mu.Unlock()
				return true
			}
			event, data = "", ""
		}
	}
	return false
}

func (f *resultFollower) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

func (f *resultFollower) wait(t *testing.T) (*clusterResult, []int, int) {
	t.Helper()
	<-f.done
	if f.err != nil {
		t.Fatal(f.err)
	}
	return f.result, f.items, f.reconnects
}

// consecutive reports whether items is 1, 2, ..., n.
func consecutive(items []int) bool {
	for i, n := range items {
		if n != i+1 {
			return false
		}
	}
	return true
}

// waitItems waits until every follower has at least n items.
func waitItems(t *testing.T, n int, fs ...*resultFollower) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for _, f := range fs {
		for f.count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("items didn't arrive (have %d)", f.count())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// twoStreamingRuns starts one long streamed run per worker (each worker runs
// one at a time) and follows both.
func twoStreamingRuns(t *testing.T, n int) (sp [2]string, runs [2]Run, fs [2]*resultFollower) {
	t.Helper()
	for i := range 2 {
		sp[i] = space(t, "alice")
		upload(t, "alice", sp[i], "", map[string]string{"stream.ipynb": longStream})
		runs[i] = startRun(t, "alice", sp[i], "stream.ipynb", map[string]any{"n": n})
		fs[i] = followResult(t, sp[i], runs[i].ID)
	}
	waitItems(t, 2, fs[0], fs[1]) // both running, on different workers
	return sp, runs, fs
}

// A worker dies (SIGKILL) while its run streams: the run ends interrupted
// within the lease, with the items it had sent; the other worker's run
// finishes untouched; the dead worker's container is removed; the
// worker comes back and takes runs again.
func TestClusterWorkerCrashMidStream(t *testing.T) {
	needCluster(t)
	_, _, fs := twoStreamingRuns(t, 20)
	killed := time.Now()
	docker(t, "kill", container("nbsandboxd", 1))
	defer docker(t, "start", container("nbsandboxd", 1))

	var lost, kept *clusterResult
	var lostItems []int
	var ended time.Duration
	for i := range 2 {
		res, items, _ := fs[i].wait(t)
		if res.Status == "interrupted" {
			lost, lostItems, ended = res, items, time.Since(killed)
		} else {
			kept = res
		}
	}
	// The other worker's sweep removes the dead worker's container.
	assertNoRunContainers(t)
	docker(t, "start", container("nbsandboxd", 1))
	if err := waitReady(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	after := space(t, "alice")
	upload(t, "alice", after, "", map[string]string{"smoke.ipynb": ""})
	next := waitRun(t, "alice", after, startRun(t, "alice", after, "smoke.ipynb", nil).ID, 2*time.Minute)

	ok := lost != nil && kept != nil && kept.Status == "succeeded" && len(kept.Output) == 20 &&
		lost.Partial && lost.Error != nil && lost.Error.Retryable && lost.Error.Fault == "platform" &&
		len(lost.Output) == len(lostItems) && len(lostItems) >= 2 && consecutive(lostItems) &&
		ended < 30*time.Second && next.Status == "succeeded"
	observed := "no run was interrupted"
	if lost != nil && kept != nil {
		observed = fmt.Sprintf("killed worker's run: %s after %.0fs, %d items streamed, %d in its result (partial %v, retryable %v); other run: %s with %d items; next run %s",
			lost.Status, ended.Seconds(), len(lostItems), len(lost.Output), lost.Partial, lost.Error != nil && lost.Error.Retryable,
			kept.Status, len(kept.Output), next.Status)
	}
	finding(t, "cluster-worker-crash", "SIGKILL one of two workers while both stream a run",
		"its run ends interrupted (retryable) within the lease, keeping the items sent; the other run finishes; the container is removed; runs continue",
		observed, ok)
	if !ok {
		t.Fatal(observed)
	}
}

// A worker is stopped gracefully (SIGTERM): its run ends interrupted
// (shutdown); a run queued behind it is taken by the other worker.
func TestClusterWorkerStop(t *testing.T) {
	needCluster(t)
	_, _, fs := twoStreamingRuns(t, 15)
	queuedSp := space(t, "alice")
	upload(t, "alice", queuedSp, "", map[string]string{"smoke.ipynb": ""})
	queued := startRun(t, "alice", queuedSp, "smoke.ipynb", nil) // both workers are busy
	docker(t, "stop", container("nbsandboxd", 1))                // SIGTERM; the stack's grace is 5 s
	defer docker(t, "start", container("nbsandboxd", 1))

	var stopped, kept *clusterResult
	for _, f := range fs {
		res, _, _ := f.wait(t)
		if res.Status == "interrupted" {
			stopped = res
		} else {
			kept = res
		}
	}
	q := waitRun(t, "alice", queuedSp, queued.ID, 2*time.Minute)
	docker(t, "start", container("nbsandboxd", 1))
	ok := stopped != nil && kept != nil && kept.Status == "succeeded" && stopped.Error != nil &&
		stopped.Error.Kind == "shutdown" && stopped.Partial && queued.Status == "queued" && q.Status == "succeeded"
	observed := fmt.Sprintf("queued run first %s, then %s", queued.Status, q.Status)
	if stopped != nil && kept != nil {
		observed = fmt.Sprintf("stopped worker's run %s/%s with %d items (partial %v); other run %s; ", stopped.Status,
			stopped.Error.Kind, len(stopped.Output), stopped.Partial, kept.Status) + observed
	}
	finding(t, "cluster-worker-stop", "SIGTERM one of two busy workers, with a third run queued",
		"its run ends interrupted (shutdown) with its items; the other run finishes; the queued run runs on the other worker",
		observed, ok)
	if !ok {
		t.Fatal(observed)
	}
}

// Both API processes crash in turn while a client follows a streamed
// result through the load balancer: the client reconnects with
// Last-Event-ID and gets every item exactly once, then the result.
func TestClusterAPICrashDuringStream(t *testing.T) {
	needCluster(t)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"stream.ipynb": longStream})
	run := startRun(t, "alice", sp, "stream.ipynb", map[string]any{"n": 15})
	f := followResult(t, sp, run.ID)
	waitItems(t, 3, f)
	for _, n := range []int{1, 2} { // whichever node serves the stream dies at some point
		docker(t, "kill", container("api", n))
		time.Sleep(time.Second)
		docker(t, "start", container("api", n))
		time.Sleep(3 * time.Second)
	}
	res, items, reconnects := f.wait(t)
	ok := res.Status == "succeeded" && len(items) == 15 && consecutive(items) && reconnects >= 1 && len(res.Output) == 15
	finding(t, "cluster-api-crash", "kill each API process in turn while a client follows a streamed result through the load balancer",
		"the client reconnects with Last-Event-ID and gets items 1..15 exactly once, then the result",
		fmt.Sprintf("%d items (in order, no duplicates: %v) over %d reconnects; result %s with %d items", len(items), consecutive(items),
			reconnects, res.Status, len(res.Output)), ok)
	if !ok {
		t.Fatalf("items %v, result %+v", items, res)
	}
}

// PostgreSQL goes away for a few seconds (less than the lease) while runs
// work: they finish normally, nothing is lost from their streams, and new
// runs are accepted once it is back.
func TestClusterPostgresBlip(t *testing.T) {
	needCluster(t)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"stream.ipynb": longStream})
	run := startRun(t, "alice", sp, "stream.ipynb", map[string]any{"n": 12})
	f := followResult(t, sp, run.ID)
	waitItems(t, 2, f)
	docker(t, "kill", container("postgres", 1))
	time.Sleep(2 * time.Second)
	docker(t, "start", container("postgres", 1))
	res, items, _ := f.wait(t)
	if err := waitReady(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	after := space(t, "alice")
	upload(t, "alice", after, "", map[string]string{"smoke.ipynb": ""})
	next := waitRun(t, "alice", after, startRun(t, "alice", after, "smoke.ipynb", nil).ID, 2*time.Minute)
	statuses := runStatuses(t, sp, run.ID)
	ok := res.Status == "succeeded" && len(items) == 12 && consecutive(items) && len(res.Output) == 12 &&
		strings.Join(statuses, ",") == "queued,running,succeeded" && next.Status == "succeeded"
	finding(t, "cluster-postgres-blip", "kill PostgreSQL for about 3 s (under the 10 s lease) while a run streams",
		"the run finishes with every item once; its status events are complete; new runs work",
		fmt.Sprintf("run %s, %d items in order %v, %d in the result; status events %v; next run %s", res.Status, len(items),
			consecutive(items), len(res.Output), statuses, next.Status), ok)
	if !ok {
		t.Fatalf("items %v result %+v statuses %v", items, res, statuses)
	}
}

// runStatuses replays a finished run's status events.
func runStatuses(t *testing.T, sp, runID string) []string {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/v1/spaces/"+sp+"/runs/"+runID+"/events", nil)
	req.SetBasicAuth("alice", passwords["alice"])
	resp, err := streamClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			var ev struct{ Status string }
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev)
			out = append(out, ev.Status)
		}
	}
	return out
}
