// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
	"github.com/azbyluthfan/nbsandbox/internal/report"
)

// singleRun runs a workload notebook in a new space of A's and waits for it.
func (e *Env) singleRun(ctx context.Context, notebook string) (Run, error) {
	sp, err := e.space(ctx, e.A, map[string][]byte{notebook: workload(notebook)})
	if err != nil {
		return Run{}, err
	}
	run, err := e.Client.StartRun(ctx, e.A, sp, notebook, nil)
	if err != nil {
		return run, err
	}
	return e.Client.WaitRun(ctx, e.A, sp, run.ID, e.waitTimeout(2*time.Minute))
}

// Cancel during a long-running cell.
var cancelRunningCheck = &Check{Name: "cancel-running", Findings: []string{"cancel-running"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		sp, err := e.space(ctx, e.A, map[string][]byte{"long.ipynb": workload("long.ipynb")})
		if err != nil {
			return nil, err
		}
		run, err := e.Client.StartRun(ctx, e.A, sp, "long.ipynb", nil)
		if err != nil {
			return nil, err
		}
		if err := e.Client.WaitLog(ctx, e.A, sp, run.ID, "tick 3", 2*time.Minute); err != nil {
			return nil, err
		}
		start := time.Now()
		if _, err := apiclient.Expect(e.Client.Cancel(ctx, e.A, sp, run.ID))(http.StatusAccepted, "cancel"); err != nil {
			return nil, err
		}
		if run, err = e.Client.WaitRun(ctx, e.A, sp, run.ID, time.Minute); err != nil {
			return nil, err
		}
		took := time.Since(start)
		left, asked := e.containersLeft(ctx, run.ID)
		// On Kubernetes, signalling the runner and copying its outputs out each
		// go through pods/exec, and the kubelet reports the stop a little late.
		margin := 5 * time.Second
		if e.Kubernetes {
			margin = 15 * time.Second
		}
		partial := run.Result != nil && run.Result.Partial
		gone := "not checked (no access to the platform)"
		if asked {
			gone = fmt.Sprint(len(left) == 0)
		}
		ok := run.Status == "cancelled" && partial && len(left) == 0 && took < 5*time.Second+margin
		return []report.Finding{c.finding("cancel-running", "cancel during a long cell",
			fmt.Sprintf("container gone within grace + %s; cancelled; partial output", margin),
			fmt.Sprintf("%s in %.1fs, partial %v, container gone %s", run.Status, took.Seconds(), partial, gone), ok)}, nil
	}}

// Cancel at the moment of completion, a few times: the API, result.json and
// the log agree on one terminal status. (The 200-round version runs as a
// unit test with exact timing control, internal/runs TestCancelCompletionRace.)
var cancelAtCompletionCheck = &Check{Name: "cancel-at-completion", Findings: []string{"cancel-at-completion"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		sp, err := e.space(ctx, e.A, map[string][]byte{"smoke.ipynb": workload("smoke.ipynb")})
		if err != nil {
			return nil, err
		}
		counts := map[string]int{}
		consistent := true
		var problems []string
		for i := range 5 {
			run, err := e.Client.StartRun(ctx, e.A, sp, "smoke.ipynb", nil)
			if err != nil {
				return nil, err
			}
			if err := e.Client.WaitLog(ctx, e.A, sp, run.ID, "artifacts written", 2*time.Minute); err != nil { // the last cell
				return nil, err
			}
			_, _ = e.Client.Cancel(ctx, e.A, sp, run.ID)
			if run, err = e.Client.WaitRun(ctx, e.A, sp, run.ID, time.Minute); err != nil {
				return nil, err
			}
			log, _ := e.Client.Log(ctx, e.A, sp, run.ID)
			if run.Result == nil || run.Result.Status != run.Status || !strings.Contains(log, "run finished: "+run.Status) {
				consistent = false
				problems = append(problems, fmt.Sprintf("round %d: api %s, log tail %q", i, run.Status, tail(log, 80)))
			}
			counts[run.Status]++
		}
		observed := fmt.Sprintf("%v, consistent %v", counts, consistent)
		if len(problems) > 0 {
			observed += "; " + strings.Join(problems, "; ")
		}
		return []report.Finding{c.finding("cancel-at-completion", "cancel right as the last cell finishes (5 rounds; 200 in the unit test)",
			"exactly one terminal status, the same in API, result.json and log", observed, consistent)}, nil
	}}

// The caller disconnects right after submitting.
var callerDisconnectCheck = &Check{Name: "caller-disconnect", Findings: []string{"caller-disconnect"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		sp, err := e.space(ctx, e.A, map[string][]byte{"smoke.ipynb": workload("smoke.ipynb")})
		if err != nil {
			return nil, err
		}
		if err := e.submitAndHangUp(ctx, sp, `{"notebook":"smoke.ipynb"}`); err != nil {
			return nil, err
		}
		var list struct{ Items []Run }
		deadline := time.Now().Add(30 * time.Second)
		for len(list.Items) == 0 && time.Now().Before(deadline) {
			_ = apiclient.Sleep(ctx, 200*time.Millisecond)
			if r, err := e.Client.Do(ctx, e.A, "GET", "/v1/spaces/"+sp+"/runs", nil, nil); err == nil {
				_ = r.JSON(&list)
			}
		}
		if len(list.Items) == 0 {
			return nil, fmt.Errorf("the run was never created")
		}
		run, err := e.Client.WaitRun(ctx, e.A, sp, list.Items[0].ID, e.waitTimeout(2*time.Minute))
		if err != nil {
			return nil, err
		}
		return []report.Finding{c.finding("caller-disconnect", "caller disconnects right after submitting", "run completes normally",
			run.Status, run.Status == "succeeded")}, nil
	}}

// submitAndHangUp writes a run request on a raw connection (TLS for https)
// and closes it without reading the answer.
func (e *Env) submitAndHangUp(ctx context.Context, spaceID, body string) error {
	u, err := url.Parse(e.Client.BaseURL)
	if err != nil {
		return err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var conn net.Conn
	d := &net.Dialer{Timeout: 10 * time.Second}
	if u.Scheme == "https" {
		cfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
		if t, ok := e.Client.HTTP.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
			cfg = t.TLSClientConfig.Clone()
			cfg.ServerName = u.Hostname()
		}
		conn, err = (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", host)
	} else {
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(e.Client.BaseURL, "/")+"/v1/spaces/"+spaceID+"/runs", strings.NewReader(body))
	if e.A.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.A.Token)
	} else {
		req.SetBasicAuth(e.A.Name, e.A.Password)
	}
	req.Header.Set("Content-Type", "application/json")
	werr := req.Write(conn)
	_ = conn.Close() // gone before reading the answer
	return werr
}

// A crash in a C extension.
var kernelCrashCheck = &Check{Name: "kernel-crash", Findings: []string{"kernel-crash"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		run, err := e.singleRun(ctx, "crash.ipynb")
		if err != nil {
			return nil, err
		}
		return []report.Finding{c.finding("kernel-crash", "crash in a C extension (ctypes)", "kernel_died", run.Status,
			run.Status == "kernel_died" && run.Fault == "notebook")}, nil
	}}

// An exception in cell 3.
var cellExceptionCheck = &Check{Name: "cell-exception", Findings: []string{"cell-exception"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		run, err := e.singleRun(ctx, "fail_cell3.ipynb")
		if err != nil {
			return nil, err
		}
		var observed string
		ok := false
		if run.Result != nil && run.Result.Error != nil {
			er := run.Result.Error
			cell := -1
			if er.CellIndex != nil {
				cell = *er.CellIndex
			}
			observed = fmt.Sprintf("%s, cell %d, %s: %s", run.Status, cell, er.ExceptionType, er.Message)
			ok = run.Status == "failed" && cell == 3 && er.ExceptionType == "ValueError" && strings.Contains(er.Message, "boom in cell 3")
		} else {
			observed = run.Status + ", no error recorded"
		}
		return []report.Finding{c.finding("cell-exception", "exception raised in cell 3", "failed, cell 3, ValueError", observed, ok)}, nil
	}}

// HTML rendering fails on a successful run.
var renderFailureCheck = &Check{Name: "render-failure", Findings: []string{"render-failure"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		run, err := e.singleRun(ctx, "render_fail.ipynb")
		if err != nil {
			return nil, err
		}
		rendered := run.Result != nil && run.Result.Rendered
		return []report.Finding{c.finding("render-failure", "HTML rendering fails on a successful run", "succeeded, rendered false",
			fmt.Sprintf("%s, rendered %v", run.Status, rendered), run.Status == "succeeded" && run.Result != nil && !rendered)}, nil
	}}
