// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Parameters, an uploaded CSV, a summary artifact.
func TestParametersCSVArtifact(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"f1_params_csv.ipynb": ""})
	upload(t, "alice", sp, "data", map[string]string{"sales.csv": "region,month,value\njakarta,1,10\njakarta,2,20\nbandung,1,5\njakarta,9,99\n"})
	run := startRun(t, "alice", sp, "f1_params_csv.ipynb", map[string]any{"region": "jakarta", "months": 6})
	run = waitRun(t, "alice", sp, run.ID, 2*time.Minute)

	log := string(call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", "alice", nil, nil).Body)
	art := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/artifacts/summary.csv", "alice", nil, nil)
	nb := string(call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/output.ipynb", "alice", nil, nil).Body)
	ok := run.Status == "succeeded" && strings.Contains(log, "region=jakarta months=6 rows=2") &&
		strings.TrimSpace(string(art.Body)) == "region,value\njakarta,30" && strings.Contains(nb, "injected-parameters")
	finding(t, "parameters-and-artifacts", "run with parameters on an uploaded CSV", "succeeded; parameters applied; summary.csv artifact",
		fmt.Sprintf("%s; artifact %q", run.Status, art.Body), ok)
	if !ok {
		t.Fatalf("run %+v\nlog:\n%s", run, log)
	}
	// The kernel talks to papermill over Unix sockets in /tmp: no TCP
	// warning in the log, and no socket files left in the user's space.
	if strings.Contains(log, "without encryption") {
		t.Errorf("kernel TCP warning in the run log:\n%s", log)
	}
	files := string(call(t, "GET", "/v1/spaces/"+sp+"/files", "alice", nil, nil).Body)
	if strings.Contains(files, "kernel") || strings.Contains(files, "-ipc") {
		t.Errorf("kernel socket files in the space: %s", files)
	}
}

// /v1/runtime matches the running image.
func TestRuntime(t *testing.T) {
	var info struct {
		Image struct {
			Ref, ID string
		}
		PythonVersion string            `json:"python_version"`
		Packages      map[string]string `json:"packages"`
		Runtime       string
	}
	r := call(t, "GET", "/v1/runtime", "alice", nil, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("runtime: %d %s", r.Code, r.Body)
	}
	r.json(t, &info)
	imageID := docker(t, "image", "inspect", info.Image.Ref, "--format", "{{.Id}}")
	installed := docker(t, "run", "--rm", "--network", "none", "--entrypoint", "python", info.Image.Ref, "-c",
		"import importlib.metadata as m, json; print(json.dumps({d.metadata['Name'].lower().replace('_','-'): d.version for d in m.distributions()}))")
	var actual map[string]string
	_ = json.Unmarshal([]byte(installed), &actual)
	mismatch := 0
	for name, v := range actual {
		if info.Packages[name] != v {
			mismatch++
		}
	}
	// On Kubernetes the ID is the node's (containerd's), not the local Docker's.
	idMatch := info.Image.ID == imageID || (onKubernetes && info.Image.ID != "")
	ok := idMatch && len(actual) > 0 && len(actual) == len(info.Packages) && mismatch == 0 && info.PythonVersion != ""
	finding(t, "runtime-info", "GET /v1/runtime", "image id and package list match the image",
		fmt.Sprintf("id match %v, %d packages, %d mismatches", idMatch, len(info.Packages), mismatch), ok)
	if !ok {
		t.Fatalf("runtime %+v vs image %s, %d installed", info.Image, imageID, len(actual))
	}
}

// Three spaces at once with a global limit of 2.
func TestQueueWithConcurrencyLimit(t *testing.T) {
	var spaces, ids []string
	for i := 0; i < 3; i++ {
		sp := space(t, "alice")
		upload(t, "alice", sp, "", map[string]string{"stream.ipynb": ""})
		spaces = append(spaces, sp)
		ids = append(ids, startRun(t, "alice", sp, "stream.ipynb", nil).ID)
	}
	time.Sleep(2 * time.Second)
	var running, queued, pos int
	for i := range ids {
		run := getRun(t, "alice", spaces[i], ids[i])
		switch run.Status {
		case "running":
			running++
		case "queued":
			queued++
			pos = run.QueuePosition
		}
	}
	var done int
	for i := range ids {
		if waitRun(t, "alice", spaces[i], ids[i], 3*time.Minute).Status == "succeeded" {
			done++
		}
	}
	ok := running == 2 && queued == 1 && pos == 1 && done == 3
	finding(t, "queue-concurrency", "three spaces start runs at once, limit 2", "two run, one queued at position 1, all complete",
		fmt.Sprintf("%d running, %d queued (position %d), %d succeeded", running, queued, pos, done), ok)
	if !ok {
		t.Fatal("queueing did not behave as specified")
	}
}

// Restart nbsandboxd mid-run.
func TestRestartMidRun(t *testing.T) {
	needCompose(t)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"long.ipynb": ""})
	run := startRun(t, "alice", sp, "long.ipynb", nil)
	waitLog(t, "alice", sp, run.ID, "tick 2", time.Minute)

	compose(t, "kill", "-s", "SIGKILL", "nbsandboxd") // a crash: no graceful shutdown
	compose(t, "up", "-d", "nbsandboxd")
	if err := waitReady(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	run = waitRun(t, "alice", sp, run.ID, time.Minute)
	assertNoRunContainers(t)
	ok := run.Status == "interrupted" && run.Fault == "platform"
	finding(t, "restart-mid-run", "kill and restart nbsandboxd while a run is active", "orphan container removed; run interrupted",
		fmt.Sprintf("%s (%s)", run.Status, run.ErrorKind), ok)
	if !ok {
		t.Fatalf("run %+v", run)
	}
}

// SIGTERM during a run → graceful shutdown.
func TestGracefulShutdown(t *testing.T) {
	needCompose(t)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"long.ipynb": ""})
	run := startRun(t, "alice", sp, "long.ipynb", nil)
	waitLog(t, "alice", sp, run.ID, "tick 1", time.Minute)

	start := time.Now()
	compose(t, "stop", "nbsandboxd") // SIGTERM; make test-integration sets a 5 s grace
	stopped := time.Since(start)
	left := runContainers(t)
	compose(t, "start", "nbsandboxd")
	if err := waitReady(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	run = getRun(t, "alice", sp, run.ID)
	ok := run.Status == "interrupted" && run.ErrorKind == "shutdown" && len(left) == 0
	finding(t, "graceful-shutdown", "SIGTERM to nbsandboxd during a run", "run stopped as interrupted (shutdown); no containers left",
		fmt.Sprintf("%s/%s after %.1fs; containers left %v", run.Status, run.ErrorKind, stopped.Seconds(), left), ok)
	if !ok {
		t.Fatalf("run %+v, left %v", run, left)
	}
}

// Deleting a space removes files, runs and artifacts.
func TestDeleteSpace(t *testing.T) {
	r := jsonCall(t, "POST", "/v1/spaces", "alice", map[string]string{"name": "to delete"}, nil)
	var sp struct{ ID string }
	r.json(t, &sp)
	upload(t, "alice", sp.ID, "", map[string]string{"smoke.ipynb": ""})
	run := waitRun(t, "alice", sp.ID, startRun(t, "alice", sp.ID, "smoke.ipynb", nil).ID, 2*time.Minute)
	del := call(t, "DELETE", "/v1/spaces/"+sp.ID, "alice", nil, nil)
	codes := []int{
		call(t, "GET", "/v1/spaces/"+sp.ID, "alice", nil, nil).Code,
		call(t, "GET", "/v1/spaces/"+sp.ID+"/runs/"+run.ID, "alice", nil, nil).Code,
		call(t, "GET", "/v1/spaces/"+sp.ID+"/runs/"+run.ID+"/artifacts/summary.csv", "alice", nil, nil).Code,
	}
	ok := run.Status == "succeeded" && del.Code == http.StatusNoContent && codes[0] == 404 && codes[1] == 404 && codes[2] == 404
	finding(t, "delete-space", "delete a space with a finished run", "space, run and artifacts return 404", fmt.Sprintf("delete %d, then %v", del.Code, codes), ok)
	if !ok {
		t.Fatal("space not fully deleted")
	}
	assertNoRunContainers(t)
}

// Cancel a queued run.
func TestCancelQueued(t *testing.T) {
	var busy []string
	var busyRuns []string
	for i := 0; i < 2; i++ {
		sp := space(t, "alice")
		upload(t, "alice", sp, "", map[string]string{"long.ipynb": ""})
		busy = append(busy, sp)
		busyRuns = append(busyRuns, startRun(t, "alice", sp, "long.ipynb", nil).ID)
	}
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"smoke.ipynb": ""})
	run := startRun(t, "alice", sp, "smoke.ipynb", nil)
	time.Sleep(time.Second)
	queued := getRun(t, "alice", sp, run.ID).Status == "queued"
	r := call(t, "POST", "/v1/spaces/"+sp+"/runs/"+run.ID+"/cancel", "alice", nil, nil)
	for i := range busy {
		call(t, "POST", "/v1/spaces/"+busy[i]+"/runs/"+busyRuns[i]+"/cancel", "alice", nil, nil)
	}
	time.Sleep(3 * time.Second) // the freed slots would pick it up now if it were still queued
	run = getRun(t, "alice", sp, run.ID)
	ok := queued && r.Code == http.StatusAccepted && run.Status == "cancelled" && run.StartedAt == nil
	finding(t, "cancel-queued", "cancel a queued run", "never starts; cancelled", fmt.Sprintf("was queued %v; %s; started %v", queued, run.Status, run.StartedAt != nil), ok)
	if !ok {
		t.Fatalf("run %+v", run)
	}
}

// Docker (the socket proxy) goes away mid-run.
func TestDockerProxyStopsMidRun(t *testing.T) {
	needCompose(t)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"long.ipynb": ""})
	run := startRun(t, "alice", sp, "long.ipynb", nil)
	waitLog(t, "alice", sp, run.ID, "tick 2", time.Minute)
	compose(t, "stop", "socket-proxy")
	time.Sleep(3 * time.Second)
	notReady := call(t, "GET", "/readyz", "", nil, nil).Code == http.StatusServiceUnavailable
	submitWhileDown := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]string{"notebook": "long.ipynb"}, nil).Code
	compose(t, "start", "socket-proxy")
	if err := waitReady(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	run = waitRun(t, "alice", sp, run.ID, time.Minute)
	assertNoRunContainers(t)
	// An API node stays ready without Docker (the workers' readiness
	// follows it); runs are refused while no worker can take them.
	ok := (run.Status == "interrupted" || run.Status == "platform_error") && run.Fault == "platform" && (notReady || clustered) &&
		(submitWhileDown == http.StatusServiceUnavailable || submitWhileDown == http.StatusConflict)
	finding(t, "docker-lost-mid-run", "Docker proxy stopped mid-run", "platform_error or interrupted, never a notebook fault; container cleaned up",
		fmt.Sprintf("%s (%s, fault %s); readyz down %v; submit while down %d", run.Status, run.ErrorKind, run.Fault, notReady, submitWhileDown), ok)
	if !ok {
		t.Fatalf("run %+v", run)
	}
}

// Bob can't see or touch alice's run through any run endpoint.
func TestRunsAreInvisibleToOtherUsers(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"smoke.ipynb": ""})
	run := waitRun(t, "alice", sp, startRun(t, "alice", sp, "smoke.ipynb", nil).ID, 2*time.Minute)
	base := "/v1/spaces/" + sp + "/runs/" + run.ID
	for _, p := range []string{base, base + "/log", base + "/output.ipynb", base + "/output.html", base + "/artifacts", base + "/artifacts/summary.csv"} {
		r := call(t, "GET", p, "bob", nil, nil)
		if r.Code != http.StatusNotFound {
			t.Errorf("bob GET %s: %d", p, r.Code)
		}
	}
	if r := call(t, "POST", base+"/cancel", "bob", nil, nil); r.Code != http.StatusNotFound {
		t.Errorf("bob cancel: %d", r.Code)
	}
	if r := call(t, "DELETE", base, "bob", nil, nil); r.Code != http.StatusNotFound {
		t.Errorf("bob delete: %d", r.Code)
	}
	html := call(t, "GET", base+"/output.html", "alice", nil, nil)
	if html.Code != http.StatusOK || !strings.Contains(html.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("html view: %d %v", html.Code, html.Header)
	}
	_, _ = io.Discard.Write(html.Body)
}
