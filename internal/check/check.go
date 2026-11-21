// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package check holds the isolation, resource and failure checks that run
// against any nbsandbox deployment through its public API, as two users:
// the integration suite runs them against the local stacks, and
// `nbsandbox-report check` against a deployment someone runs. Each check
// returns the report's findings.
//
// Nothing here is destructive to the deployment: no restarts, no stopping
// services. Those tests stay in the integration suite.
package check

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
	"github.com/azbyluthfan/nbsandbox/internal/report"
	"github.com/azbyluthfan/nbsandbox/tests/probes"
	"github.com/azbyluthfan/nbsandbox/tests/workloads"
)

// Env is the deployment under check.
type Env struct {
	Client *Client
	// A and B are two different users: A probes, B is the neighbour.
	A, B *Identity
	// Kubernetes: runs are Pods (from /v1/runtime, or set by the caller).
	Kubernetes bool
	// ExtraTargets are host:port pairs a run must not reach besides the
	// internet and the metadata service (on Kubernetes: the API server, a
	// worker Pod, the node's kubelet).
	ExtraTargets []string
	// DataDir is the server's data folder, when known: the probe also
	// tries its paths.
	DataDir string
	// Limits, from /v1/runtime (Load fills them).
	RunTimeout time.Duration
	LogBytes   int64
	Runtime    string
	// Info is /v1/runtime without the package list (for the report).
	Info map[string]any

	// Hooks the integration suite supplies; nil elsewhere.
	// RunLeft lists what is left of the run's container or Pod (the
	// integration suite lists every run container: its stack is private).
	RunLeft func(ctx context.Context, runID string) ([]string, error)
	// PlatformLogBytes is how much the container platform kept of a run's
	// output itself (Docker's own log); ok false when it can't be measured.
	PlatformLogBytes func(ctx context.Context, runID string) (n int, ok bool)
	// Logf reports progress (nil: quiet).
	Logf func(format string, args ...any)

	mu       sync.Mutex
	cleanups []func(context.Context)
}

// Load reads the deployment's limits and platform from /v1/runtime.
func (e *Env) Load(ctx context.Context) error {
	r, err := apiclient.Expect(e.Client.Do(ctx, e.A, "GET", "/v1/runtime", nil, nil))(http.StatusOK, "GET /v1/runtime")
	if err != nil {
		return err
	}
	var info struct {
		Runtime string `json:"runtime"`
		Limits  struct {
			TimeoutSeconds float64 `json:"timeout_seconds"`
			LogBytes       int64   `json:"log_bytes"`
		} `json:"limits"`
		Docker struct {
			OperatingSystem string `json:"operating_system"`
		} `json:"docker"`
	}
	if err := r.JSON(&info); err != nil {
		return err
	}
	if err := r.JSON(&e.Info); err != nil {
		return err
	}
	delete(e.Info, "packages")
	delete(e.Info, "modules")
	e.Runtime = info.Runtime
	if e.Runtime == "" {
		e.Runtime = "runc"
	}
	e.RunTimeout = time.Duration(info.Limits.TimeoutSeconds * float64(time.Second))
	e.LogBytes = info.Limits.LogBytes
	if strings.HasPrefix(info.Docker.OperatingSystem, "Kubernetes") {
		e.Kubernetes = true
	}
	if _, err := apiclient.Expect(e.Client.Do(ctx, e.B, "GET", "/v1/runtime", nil, nil))(http.StatusOK, "GET /v1/runtime as the second user"); err != nil {
		return err
	}
	return nil
}

func (e *Env) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// space creates a space for who holding files, deleted by Cleanup.
func (e *Env) space(ctx context.Context, who *Identity, files map[string][]byte) (string, error) {
	id, err := e.Client.CreateSpace(ctx, who, "nbsandbox-check-"+randomHex(4))
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.cleanups = append(e.cleanups, func(ctx context.Context) {
		if err := e.Client.DeleteSpace(ctx, who, id); err != nil {
			e.logf("cleaning up space %s: %v", id, err)
		}
	})
	e.mu.Unlock()
	if len(files) > 0 {
		if err := e.Client.Upload(ctx, who, id, files); err != nil {
			return id, err
		}
	}
	return id, nil
}

// Cleanup deletes every space the checks created (their runs cancelled
// first). It doesn't use ctx's cancellation, so it also works after Ctrl-C.
func (e *Env) Cleanup(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	e.mu.Lock()
	fns := e.cleanups
	e.cleanups = nil
	e.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i](ctx)
	}
}

// waitTimeout is how long to wait for a run that should end on its own:
// longer on a deployment with a longer run timeout.
func (e *Env) waitTimeout(d time.Duration) time.Duration {
	return max(d, e.RunTimeout+time.Minute)
}

// containersLeft lists what is left of the run's container, and false when
// the deployment can't be asked (then the check says so).
func (e *Env) containersLeft(ctx context.Context, runID string) ([]string, bool) {
	if e.RunLeft == nil {
		return nil, false
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		left, err := e.RunLeft(ctx, runID)
		if err == nil && len(left) == 0 {
			return nil, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return left, true
		}
		_ = apiclient.Sleep(ctx, 500*time.Millisecond)
	}
}

// workload returns a notebook from tests/workloads, probe one from tests/probes.
func workload(name string) []byte { return mustRead(workloads.FS, name) }
func probe(name string) []byte    { return mustRead(probes.FS, name) }

func mustRead(f fs.FS, name string) []byte {
	b, err := fs.ReadFile(f, name)
	if err != nil {
		panic(fmt.Sprintf("embedded notebook %s: %v", name, err)) // a build error, not a runtime one
	}
	return b
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// A Check is one scenario: it produces one or more findings.
type Check struct {
	Name     string   // the scenario, also the findings' Test
	Findings []string // the finding IDs it produces
	// Slow: takes about the deployment's run timeout (skippable).
	Slow bool
	run  func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error)
}

func (c *Check) finding(id, attempted, expected, observed string, pass bool) report.Finding {
	return report.Finding{ID: id, Attempted: attempted, Expected: expected, Observed: observed, Pass: pass,
		Test: c.Name, At: time.Now().UTC()}
}

// Run runs the check and deletes what it created. An error means the check
// couldn't run (its findings are then missing from the report).
func (c *Check) Run(ctx context.Context, e *Env) ([]report.Finding, error) {
	defer e.Cleanup(ctx)
	fs, err := c.run(ctx, e, c)
	if err != nil {
		return fs, fmt.Errorf("%s: %w", c.Name, err)
	}
	return fs, nil
}

// All is every check, in the report's order.
func All() []*Check {
	return []*Check{isolationCheck, memoryCheck, cpuCheck, pidsCheck, tmpCheck, endlessCheck,
		cancelRunningCheck, cancelAtCompletionCheck, callerDisconnectCheck, kernelCrashCheck, cellExceptionCheck, renderFailureCheck, timeLimitCheck,
		sessionsCheck}
}

// Get returns the check called name.
func Get(name string) *Check {
	for _, c := range All() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// NotChecked are the report's findings only the integration suite produces
// (they stop services, or need the test stack's settings), with why.
var NotChecked = map[string]string{
	"cancel-queued":       "needs every run slot of the deployment busy",
	"docker-lost-mid-run": "stops the Docker socket proxy",
}
