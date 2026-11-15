// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// A space's own packages, end to end, against the real package index
// (make test-packages: the dev stack with packages.enabled, and network
// for the build). Skipped unless NBSANDBOX_TEST_PACKAGES=1.

func needPackages(t *testing.T) {
	t.Helper()
	if os.Getenv("NBSANDBOX_TEST_PACKAGES") != "1" {
		t.Skip("set NBSANDBOX_TEST_PACKAGES=1 (make test-packages)")
	}
}

type spacePackages struct {
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	BuildLog  string `json:"build_log"`
	SizeBytes int64  `json:"size_bytes"`
	Installed []struct {
		Name, Version string
	} `json:"installed"`
}

func packagesOf(t *testing.T, sp string) spacePackages {
	t.Helper()
	var p spacePackages
	call(t, "GET", "/v1/spaces/"+sp+"/packages", "alice", nil, nil).json(t, &p)
	return p
}

// A run imports a package the runner image doesn't have, once the space's
// requirements.txt is built; the next run doesn't wait; a published
// version keeps the set; the run still has no network.
func TestPackagesEndToEnd(t *testing.T) {
	needPackages(t)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{
		"requirements.txt": "humanize\ntabulate\npandas\n", // pandas: the image's, skipped
		"pkgs.ipynb": codeNotebook(
			"import humanize, tabulate, pandas, nbsandbox\nprint(humanize.__file__)",
			"import socket\ntry:\n    socket.create_connection(('pypi.org', 443), timeout=3)\n    net = True\nexcept OSError:\n    net = False",
			"nbsandbox.output({'naturalsize': humanize.naturalsize(1_000_000), 'table': tabulate.tabulate([[1, 2]]), 'network': net})"),
	})
	start := time.Now()
	first := startRun(t, "alice", sp, "pkgs.ipynb", nil)
	res := runResult(t, "alice", sp, first.ID)
	firstTook := time.Since(start)
	if res.Status != "succeeded" || !strings.Contains(string(res.Output), `"naturalsize":"1.0 MB"`) || !strings.Contains(string(res.Output), `"network":false`) {
		t.Fatalf("first run: %+v %s", res, res.Output)
	}
	log := string(call(t, "GET", "/v1/spaces/"+sp+"/runs/"+first.ID+"/log", "alice", nil, nil).Body)
	if !strings.Contains(log, "/opt/nbsandbox/packages/humanize") {
		t.Errorf("humanize not from the package set: %s", log)
	}
	p := packagesOf(t, sp)
	names := map[string]bool{}
	for _, i := range p.Installed {
		names[i.Name] = true
	}
	if p.Status != "ready" || !names["humanize"] || !names["tabulate"] || names["pandas"] {
		t.Fatalf("set: %+v", p)
	}

	start = time.Now()
	second := startRun(t, "alice", sp, "pkgs.ipynb", nil)
	if res := runResult(t, "alice", sp, second.ID); res.Status != "succeeded" {
		t.Fatalf("second run: %+v", res)
	}
	secondTook := time.Since(start)

	if r := call(t, "POST", "/v1/spaces/"+sp+"/functions/pkgs/versions", "alice", strings.NewReader(`{"notebook":"pkgs.ipynb"}`),
		map[string]string{"Content-Type": "application/json"}); r.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", r.Code, r.Body)
	}
	start = time.Now()
	r := call(t, "POST", "/v1/spaces/"+sp+"/functions/pkgs/invoke?wait=120", "alice", strings.NewReader(`{"parameters":{}}`),
		map[string]string{"Content-Type": "application/json"})
	invokeTook := time.Since(start)
	if r.Code != http.StatusOK || !strings.Contains(string(r.Body), "1.0 MB") {
		t.Fatalf("invoke: %d %s", r.Code, r.Body)
	}
	finding(t, "packages-end-to-end", "a space's requirements.txt (humanize, tabulate, pandas) is built into a package set; runs import it; a published version keeps it",
		"the run imports humanize from the set, still without network; pandas stays the image's",
		fmt.Sprintf("first run %.1fs (with the build), next run %.1fs, first invocation %.1fs (its locked set), set %d KiB",
			firstTook.Seconds(), secondTook.Seconds(), invokeTook.Seconds(), p.SizeBytes>>10), true)
}

// Requirements that can't be built fail the waiting run with the reason,
// and the next run is refused at once.
func TestPackagesFailures(t *testing.T) {
	needPackages(t)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{
		"requirements.txt": "this-package-does-not-exist-nbsandbox-test\n",
		"nb.ipynb":         codeNotebook("print('never')"),
	})
	run := startRun(t, "alice", sp, "nb.ipynb", nil)
	done := waitRun(t, "alice", sp, run.ID, 5*time.Minute)
	if done.Status != "failed" || done.ErrorKind != "packages" {
		t.Fatalf("waiting run: %+v", done)
	}
	if r := call(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`),
		map[string]string{"Content-Type": "application/json"}); r.Code != http.StatusUnprocessableEntity || !strings.Contains(string(r.Body), "packages_failed") {
		t.Fatalf("next run: %d %s", r.Code, r.Body)
	}
	if p := packagesOf(t, sp); p.Status != "failed" || !strings.Contains(p.BuildLog, "No matching distribution") {
		t.Errorf("shown: %+v", p)
	}

	// A conflict with the runner image's own packages.
	upload(t, "alice", sp, "", map[string]string{"requirements.txt": "pandas==2.0.0\n"})
	if r := call(t, "POST", "/v1/spaces/"+sp+"/packages", "alice", nil, nil); r.Code != http.StatusAccepted {
		t.Fatalf("build: %d %s", r.Code, r.Body)
	}
	deadline := time.Now().Add(5 * time.Minute)
	var p spacePackages
	for p = packagesOf(t, sp); p.Status == "pending" || p.Status == "building"; p = packagesOf(t, sp) {
		if time.Now().After(deadline) {
			t.Fatal("build didn't finish")
		}
		time.Sleep(time.Second)
	}
	if p.Status != "failed" || !strings.Contains(p.BuildLog, "pandas") {
		t.Errorf("conflict: %+v", p)
	}
}
