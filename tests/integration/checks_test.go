// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/check"
)

// The isolation, resource and failure checks live in internal/check, where
// `nbsandbox-report check` runs the same code against any deployment. Here
// they run against the local stack, with what only the test stack can see:
// its run containers, Docker's own log, and responses checked against the
// API spec.

func TestIsolationSuite(t *testing.T)     { runCheck(t, "isolation") }
func TestMemoryLimit(t *testing.T)        { runCheck(t, "memory-limit") }
func TestCPUFairness(t *testing.T)        { runCheck(t, "cpu-fairness") }
func TestProcessLimit(t *testing.T)       { runCheck(t, "process-limit") }
func TestTimeout(t *testing.T)            { runCheck(t, "time-limit") }
func TestTmpFull(t *testing.T)            { runCheck(t, "tmp-limit") }
func TestEndlessOutput(t *testing.T)      { runCheck(t, "endless-output") }
func TestCancelRunning(t *testing.T)      { runCheck(t, "cancel-running") }
func TestCancelAtCompletion(t *testing.T) { runCheck(t, "cancel-at-completion") }
func TestCallerDisconnects(t *testing.T)  { runCheck(t, "caller-disconnect") }
func TestKernelCrash(t *testing.T)        { runCheck(t, "kernel-crash") }
func TestExceptionInCell3(t *testing.T)   { runCheck(t, "cell-exception") }
func TestRenderFails(t *testing.T)        { runCheck(t, "render-failure") }

// checkEnv is the local stack as the checks see it: alice probes, bob is
// the neighbour.
func checkEnv(t *testing.T) *check.Env {
	t.Helper()
	client := &check.Client{BaseURL: baseURL, HTTP: httpClient,
		Inspect: func(method, p string, resp *http.Response, body []byte) { checkSpec(t, method, p, resp, body) }}
	e := &check.Env{
		Client:  client,
		A:       &check.Identity{Name: "alice", Password: passwords["alice"]},
		B:       &check.Identity{Name: "bob", Password: passwords["bob"]},
		DataDir: envOr("NBSANDBOX_DATA_DIR", ""),
		Logf:    t.Logf,
		RunLeft: func(context.Context, string) ([]string, error) {
			return runContainers(t), nil
		},
	}
	if err := e.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Kubernetes = onKubernetes
	e.ExtraTargets = podTargets(t)
	if !onKubernetes {
		// Docker's own log of the run's container (kubelet rotates its own).
		e.PlatformLogBytes = func(_ context.Context, runID string) (int, bool) {
			out, err := exec.Command("sh", "-c", "docker logs nbsandbox-run-"+runID+" 2>&1 | wc -c").Output()
			if err != nil {
				return -1, true
			}
			n, err := strconv.Atoi(strings.TrimSpace(string(out)))
			if err != nil {
				return -1, true
			}
			return n, true
		}
	}
	check.CPUFairnessGate = os.Getenv("NBSANDBOX_CPU_FAIRNESS_GATE") == "1"
	return e
}

// runCheck runs one check, records its findings for the report, and fails
// the test on a failed finding.
func runCheck(t *testing.T, name string) {
	t.Helper()
	c := check.Get(name)
	if c == nil {
		t.Fatalf("no check %q", name)
	}
	findings, err := c.Run(context.Background(), checkEnv(t))
	for _, f := range findings {
		finding(t, f.ID, f.Attempted, f.Expected, f.Observed, f.Pass)
		if !f.Pass {
			t.Errorf("%s failed: %s", f.ID, f.Observed)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	assertNoRunContainers(t)
}
