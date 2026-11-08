// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// codeNotebook builds an nbformat 4 notebook with one code cell per source.
func codeNotebook(sources ...string) string {
	cells := make([]map[string]any, 0, len(sources))
	for _, s := range sources {
		cells = append(cells, map[string]any{"cell_type": "code", "metadata": map[string]any{}, "outputs": []any{},
			"execution_count": nil, "source": s})
	}
	b, _ := json.Marshal(map[string]any{"nbformat": 4, "nbformat_minor": 5, "metadata": map[string]any{}, "cells": cells})
	return string(b)
}

// runResult waits for a run and returns its caller result.
func runResult(t *testing.T, user, sp, runID string) callerResult {
	t.Helper()
	waitRun(t, user, sp, runID, 3*time.Minute)
	var res callerResult
	r := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+runID+"/result", user, nil, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("result: %d %s", r.Code, r.Body)
	}
	r.json(t, &res)
	return res
}

// startRunBody starts a run with a full request body and returns the
// response, so tests can check refusals too.
func startRunBody(t *testing.T, user, sp string, body map[string]any) response {
	t.Helper()
	return jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", user, body, nil)
}

// A run's artifact copied into the space becomes an ordinary file that the
// next run reads; the copy refuses to overwrite and to leave the space.
func TestPromoteArtifact(t *testing.T) {
	sp := space(t, "alice")
	producer := codeNotebook(
		"import os\npath = os.path.join(os.environ['NBSANDBOX_OUTPUT_DIR'], 'summary.csv')\nopen(path, 'w').write('region,total\\nbandung,42\\n')")
	consumer := codeNotebook(
		"import nbsandbox\nnbsandbox.output(open('reports/summary.csv').read())")
	upload(t, "alice", sp, "", map[string]string{"producer.ipynb": producer, "consumer.ipynb": consumer})

	run := startRun(t, "alice", sp, "producer.ipynb", nil)
	run = waitRun(t, "alice", sp, run.ID, 3*time.Minute)
	if run.Status != "succeeded" {
		t.Fatalf("producer: %+v", run)
	}
	var arts struct{ Items []struct{ Path string } }
	call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/artifacts", "alice", nil, nil).json(t, &arts)

	base := "/v1/spaces/" + sp + "/runs/" + run.ID + "/promote"
	first := jsonCall(t, "POST", base, "alice", map[string]string{"artifact": "summary.csv", "to": "reports/summary.csv"}, nil)
	again := jsonCall(t, "POST", base, "alice", map[string]string{"artifact": "summary.csv", "to": "reports/summary.csv"}, nil)
	missing := jsonCall(t, "POST", base, "alice", map[string]string{"artifact": "nope.csv", "to": "reports/nope.csv"}, nil)
	escape := jsonCall(t, "POST", base, "alice", map[string]string{"artifact": "summary.csv", "to": "../escape.csv"}, nil)
	fromArtifacts := jsonCall(t, "POST", base, "alice", map[string]string{"artifact": "../input.ipynb", "to": "copied.ipynb"}, nil)
	content := string(call(t, "GET", "/v1/spaces/"+sp+"/files/reports/summary.csv", "alice", nil, nil).Body)

	next := startRun(t, "alice", sp, "consumer.ipynb", nil)
	res := runResult(t, "alice", sp, next.ID)
	var got string
	_ = json.Unmarshal(res.Output, &got)

	ok := len(arts.Items) == 1 && arts.Items[0].Path == "summary.csv" &&
		first.Code == http.StatusCreated && again.Code == http.StatusConflict && missing.Code == http.StatusNotFound &&
		escape.Code == http.StatusBadRequest && fromArtifacts.Code == http.StatusBadRequest &&
		strings.Contains(content, "bandung,42") && res.Status == "succeeded" && strings.Contains(got, "bandung,42")
	finding(t, "promote-artifact", "copy a run's artifact into the space, then read it from the next run",
		"201 and the next run reads it; 409 to overwrite; 404 for a missing artifact; 400 for a target outside the space or a source outside the artifacts",
		fmt.Sprintf("artifacts %v; promote %d, again %d, missing %d, escape %d, outside artifacts %d; next run %s %q",
			arts.Items, first.Code, again.Code, missing.Code, escape.Code, fromArtifacts.Code, res.Status, got), ok)
	if !ok {
		t.Fatalf("promote %s / %s / %s / %s / %s; next %+v", first.Body, again.Body, missing.Body, escape.Body, fromArtifacts.Body, res)
	}
}

// A run started with "space": "read-only" can't change the space's files,
// and may run beside a writing run, while a second writing run is refused.
func TestReadOnlyRun(t *testing.T) {
	sp := space(t, "alice")
	slowWriter := codeNotebook("import time\nopen('from_writer.txt', 'w').write('w')\ntime.sleep(8)")
	upload(t, "alice", sp, "", map[string]string{"writer.ipynb": writer, "slow_writer.ipynb": slowWriter})

	// Alone: the read-only run's write fails.
	r := startRunBody(t, "alice", sp, map[string]any{"notebook": "writer.ipynb", "space": "read-only"})
	if r.Code != http.StatusCreated {
		t.Fatalf("read-only run: %d %s", r.Code, r.Body)
	}
	var ro Run
	r.json(t, &ro)
	roRes := runResult(t, "alice", sp, ro.ID)
	files := string(call(t, "GET", "/v1/spaces/"+sp+"/files", "alice", nil, nil).Body)
	okRO := roRes.Status == "failed" && roRes.Error != nil && strings.Contains(roRes.Error.Message, "Read-only") &&
		!strings.Contains(files, "written.txt")
	finding(t, "read-only-run", `a run with "space": "read-only" writes into /space`,
		"failed with a read-only file system error; nothing written",
		fmt.Sprintf("%s (%v); written.txt listed %v", roRes.Status, roRes.Error, strings.Contains(files, "written.txt")), okRO)

	// Beside a writing run: a read-only run starts; another writing run is 409.
	w := startRun(t, "alice", sp, "slow_writer.ipynb", nil)
	reader := startRunBody(t, "alice", sp, map[string]any{"notebook": "writer.ipynb", "space": "read-only"})
	second := startRunBody(t, "alice", sp, map[string]any{"notebook": "slow_writer.ipynb"})
	var rd Run
	if reader.Code == http.StatusCreated {
		reader.json(t, &rd)
		waitRun(t, "alice", sp, rd.ID, 3*time.Minute)
	}
	wDone := waitRun(t, "alice", sp, w.ID, 3*time.Minute)
	okBeside := reader.Code == http.StatusCreated && second.Code == http.StatusConflict && wDone.Status == "succeeded"
	finding(t, "read-only-beside-writer", "start a read-only run and a second writing run while a writing run is active",
		"the read-only run is accepted; the second writing run is 409; the first writer succeeds",
		fmt.Sprintf("read-only %d, second writer %d, first writer %s", reader.Code, second.Code, wDone.Status), okBeside)
	if !okRO || !okBeside {
		t.Fatalf("read-only %+v; beside: reader %d %s, second %d %s, writer %+v", roRes, reader.Code, reader.Body, second.Code, second.Body, wDone)
	}
}

// A run that writes its space past the quota is stopped with resource_disk,
// and the space then refuses uploads until something is deleted.
func TestDiskQuota(t *testing.T) {
	sp := space(t, "alice")
	// 600 MiB against the default 500 MiB quota, then hold, so the disk
	// watch (every 5 s) catches it before the run ends by itself.
	filler := codeNotebook(
		"chunk = b'\\0' * (8 << 20)\nwith open('big.bin', 'wb') as f:\n    for _ in range(75):\n        f.write(chunk)\n        f.flush()",
		"import time\ntime.sleep(45)")
	upload(t, "alice", sp, "", map[string]string{"filler.ipynb": filler})
	start := time.Now()
	run := startRun(t, "alice", sp, "filler.ipynb", nil)
	run = waitRun(t, "alice", sp, run.ID, 3*time.Minute)
	took := time.Since(start)
	// A run works on a copy of the space, and changes over the quota are not
	// applied: the space is as before, so uploads still work, and the run's
	// log says why its changes are missing.
	uploadAfter := uploadCode(t, sp, "after.txt", "x")
	big := call(t, "GET", "/v1/spaces/"+sp+"/files/big.bin", "alice", nil, nil)
	log := string(call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", "alice", nil, nil).Body)

	// Within seconds of going over (plus start-up, which is slow on kind).
	limit := 40 * time.Second
	if onKubernetes {
		limit = 100 * time.Second
	}
	ok := run.Status == "failed" && run.ErrorKind == "resource_disk" && took < limit &&
		uploadAfter == http.StatusCreated && big.Code == http.StatusNotFound && strings.Contains(log, "would exceed the space's quota")
	finding(t, "disk-quota", "a run writes 600 MiB into a space with a 500 MiB quota",
		"stopped as failed/resource_disk within seconds; its changes are not applied (the log says so); the space stays usable",
		fmt.Sprintf("%s/%s after %.0fs; big.bin %d; upload afterwards %d; log explains: %v",
			run.Status, run.ErrorKind, took.Seconds(), big.Code, uploadAfter, strings.Contains(log, "would exceed the space's quota")), ok)
	if !ok {
		t.Fatalf("run %+v; big.bin %d; upload %d; log tail %q", run, big.Code, uploadAfter, tail(log, 400))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// uploadCode uploads one small file and returns the status code.
func uploadCode(t *testing.T, sp, name, content string) int {
	t.Helper()
	r := call(t, "PUT", "/v1/spaces/"+sp+"/files/"+name, "alice", strings.NewReader(content),
		map[string]string{"Content-Type": "application/octet-stream", "If-None-Match": "*"})
	return r.Code
}

// A run that changes a file makes an editor's version stale: saving with
// the old version is refused (412, naming the current one), saving with the
// current one works. And a run executes the notebook as submitted, even if
// it is saved again while the run is queued or running.
func TestFileVersionsAndRuns(t *testing.T) {
	sp := space(t, "alice")
	appender := codeNotebook("import time\ntime.sleep(4)\nopen('notes.txt', 'a').write('from the run\\n')")
	upload(t, "alice", sp, "", map[string]string{"appender.ipynb": appender, "notes.txt": "mine\n"})

	opened := call(t, "GET", "/v1/spaces/"+sp+"/files/notes.txt", "alice", nil, nil).Header.Get("ETag")
	run := startRun(t, "alice", sp, "appender.ipynb", nil)

	// Saved while queued or running: the run must still execute the old copy.
	edited := codeNotebook("open('notes.txt', 'a').write('from the EDITED notebook\\n')")
	nbTag := call(t, "GET", "/v1/spaces/"+sp+"/files/appender.ipynb", "alice", nil, nil).Header.Get("ETag")
	resave := call(t, "PUT", "/v1/spaces/"+sp+"/files/appender.ipynb", "alice", strings.NewReader(edited),
		map[string]string{"Content-Type": "application/octet-stream", "If-Match": nbTag})

	done := waitRun(t, "alice", sp, run.ID, 3*time.Minute)
	var meta struct {
		NotebookSHA256 string `json:"notebook_sha256"`
	}
	call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID, "alice", nil, nil).json(t, &meta)
	sum := sha256.Sum256([]byte(appender))
	notes := string(call(t, "GET", "/v1/spaces/"+sp+"/files/notes.txt", "alice", nil, nil).Body)
	okSnapshot := resave.Code == http.StatusOK && done.Status == "succeeded" &&
		meta.NotebookSHA256 == hex.EncodeToString(sum[:]) && strings.Contains(notes, "from the run") && !strings.Contains(notes, "EDITED")
	finding(t, "run-snapshot", "save the notebook again while its run is queued or running",
		"the run executes the notebook as submitted (same hash, old behaviour)",
		fmt.Sprintf("re-save %d; run %s; hash matches %v; notes %q", resave.Code, done.Status,
			meta.NotebookSHA256 == hex.EncodeToString(sum[:]), notes), okSnapshot)

	// The editor still has the version from before the run.
	stale := call(t, "PUT", "/v1/spaces/"+sp+"/files/notes.txt", "alice", strings.NewReader("mine, edited\n"),
		map[string]string{"Content-Type": "application/octet-stream", "If-Match": opened})
	var conflict struct {
		Error          struct{ Code string } `json:"error"`
		CurrentVersion string                `json:"current_version"`
	}
	_ = json.Unmarshal(stale.Body, &conflict)
	var list struct {
		Items []struct{ Path, Version string }
	}
	call(t, "GET", "/v1/spaces/"+sp+"/files", "alice", nil, nil).json(t, &list)
	var listed string
	for _, f := range list.Items {
		if f.Path == "notes.txt" {
			listed = f.Version
		}
	}
	fresh := call(t, "PUT", "/v1/spaces/"+sp+"/files/notes.txt", "alice", strings.NewReader("mine, edited\n"),
		map[string]string{"Content-Type": "application/octet-stream", "If-Match": `"` + conflict.CurrentVersion + `"`})
	okVersions := stale.Code == http.StatusPreconditionFailed && conflict.Error.Code == "version_mismatch" &&
		conflict.CurrentVersion != "" && conflict.CurrentVersion == listed && fresh.Code == http.StatusOK
	finding(t, "version-after-run", "save a file with the version read before a run changed it",
		"412 version_mismatch naming the current version (as listed); saving with that version works",
		fmt.Sprintf("stale save %d %s (current %q, listed %q); save with current %d",
			stale.Code, conflict.Error.Code, conflict.CurrentVersion, listed, fresh.Code), okVersions)
	if !okSnapshot || !okVersions {
		t.Fatalf("snapshot %v (%s, %+v, %q); versions: stale %d %s, fresh %d %s",
			okSnapshot, resave.Body, done, notes, stale.Code, stale.Body, fresh.Code, fresh.Body)
	}
}
