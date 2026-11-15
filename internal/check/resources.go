// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
	"github.com/azbyluthfan/nbsandbox/internal/report"
)

// alongside starts A's bad notebook and B's good one at the same time and
// waits for both.
func (e *Env) alongside(ctx context.Context, bad string, badParams map[string]any, good string) (Run, Run, error) {
	spA, err := e.space(ctx, e.A, map[string][]byte{bad: workload(bad)})
	if err != nil {
		return Run{}, Run{}, err
	}
	spB, err := e.space(ctx, e.B, map[string][]byte{good: workload(good)})
	if err != nil {
		return Run{}, Run{}, err
	}
	a, err := e.Client.StartRun(ctx, e.A, spA, bad, badParams)
	if err != nil {
		return Run{}, Run{}, err
	}
	b, err := e.Client.StartRun(ctx, e.B, spB, good, nil)
	if err != nil {
		return Run{}, Run{}, err
	}
	if a, err = e.Client.WaitRun(ctx, e.A, spA, a.ID, e.waitTimeout(3*time.Minute)); err != nil {
		return a, b, err
	}
	b, err = e.Client.WaitRun(ctx, e.B, spB, b.ID, e.waitTimeout(3*time.Minute))
	return a, b, err
}

// A allocates past the memory limit while B runs normally.
var memoryCheck = &Check{Name: "memory-limit", Findings: []string{"memory-limit"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		a, b, err := e.alongside(ctx, "oom.ipynb", nil, "smoke.ipynb")
		if err != nil {
			return nil, err
		}
		var peak int64
		if a.Result != nil && a.Result.Usage.PeakMemoryBytes != nil {
			peak = *a.Result.Usage.PeakMemoryBytes
		}
		// On Kubernetes (cgroup v2, 1.28+) an OOM kill takes the whole
		// container, wrapper included, so the peak isn't reported.
		ok := a.Status == "oom" && (peak > 0 || e.Kubernetes) && b.Status == "succeeded"
		return []report.Finding{c.finding("memory-limit", "alice allocates beyond the memory limit while bob runs a normal notebook",
			"alice oom (not kernel_died/failed) with peak memory reported (not on Kubernetes, which kills the whole container); bob succeeded",
			fmt.Sprintf("alice %s (%s), peak %d MiB; bob %s", a.Status, a.Message, peak>>20, b.Status), ok)}, nil
	}}

var wallRE = regexp.MustCompile(`cpu_fixed wall=([0-9.]+)s`)

// cpuFixed runs the fixed CPU workload in a space and returns its wall time.
func (e *Env) cpuFixed(ctx context.Context, sp string) (float64, error) {
	run, err := e.Client.StartRun(ctx, e.B, sp, "cpu_fixed.ipynb", nil)
	if err != nil {
		return 0, err
	}
	if run, err = e.Client.WaitRun(ctx, e.B, sp, run.ID, e.waitTimeout(3*time.Minute)); err != nil {
		return 0, err
	}
	log, _ := e.Client.Log(ctx, e.B, sp, run.ID)
	m := wallRE.FindStringSubmatch(log)
	if run.Status != "succeeded" || m == nil {
		return 0, fmt.Errorf("cpu_fixed: %s", run.Status)
	}
	return strconv.ParseFloat(m[1], 64)
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// CPUFairnessGate makes cpu-fairness pass or fail on the 1.5× tolerance; off,
// the ratio is recorded only (shared or small hosts vary too much).
var CPUFairnessGate = false

// A spins every core while B runs fixed CPU work.
var cpuCheck = &Check{Name: "cpu-fairness", Findings: []string{"cpu-fairness"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		spB, err := e.space(ctx, e.B, map[string][]byte{"cpu_fixed.ipynb": workload("cpu_fixed.ipynb")})
		if err != nil {
			return nil, err
		}
		var alone, contended []float64
		for range 3 {
			v, err := e.cpuFixed(ctx, spB)
			if err != nil {
				return nil, err
			}
			alone = append(alone, v)
		}
		spA, err := e.space(ctx, e.A, map[string][]byte{"cpu_hog.ipynb": workload("cpu_hog.ipynb")})
		if err != nil {
			return nil, err
		}
		hog, err := e.Client.StartRun(ctx, e.A, spA, "cpu_hog.ipynb", map[string]any{"seconds": 50})
		if err != nil {
			return nil, err
		}
		if err := e.Client.WaitLog(ctx, e.A, spA, hog.ID, "Executing Cell 2", 2*time.Minute); err != nil {
			return nil, err
		}
		_ = apiclient.Sleep(ctx, 2*time.Second) // let the hog spin up
		for range 3 {
			v, err := e.cpuFixed(ctx, spB)
			if err != nil {
				return nil, err
			}
			contended = append(contended, v)
		}
		h, _ := e.Client.GetRun(ctx, e.A, spA, hog.ID)
		hogStill := h.Status == "running"
		_, _ = e.Client.Cancel(ctx, e.A, spA, hog.ID)
		_, _ = e.Client.WaitRun(ctx, e.A, spA, hog.ID, time.Minute)

		ratio := median(contended) / median(alone)
		ok := ratio <= 1.5 && hogStill
		mode := "report-only on this platform"
		if CPUFairnessGate {
			mode = "gating"
		}
		return []report.Finding{c.finding("cpu-fairness", "alice spins all cores while bob runs a fixed CPU workload",
			"bob within 1.5× of his solo baseline",
			fmt.Sprintf("bob alone %.2fs, with the hog %.2fs: %.2f× (runs alone %v, contended %v; %s)",
				median(alone), median(contended), ratio, alone, contended, mode), ok || !CPUFairnessGate)}, nil
	}}

// A starts processes until the limit refuses, while B runs normally and
// the API stays responsive.
var pidsCheck = &Check{Name: "process-limit", Findings: []string{"process-limit"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		spA, err := e.space(ctx, e.A, map[string][]byte{"pids.ipynb": workload("pids.ipynb")})
		if err != nil {
			return nil, err
		}
		spB, err := e.space(ctx, e.B, map[string][]byte{"smoke.ipynb": workload("smoke.ipynb")})
		if err != nil {
			return nil, err
		}
		a, err := e.Client.StartRun(ctx, e.A, spA, "pids.ipynb", nil)
		if err != nil {
			return nil, err
		}
		b, err := e.Client.StartRun(ctx, e.B, spB, "smoke.ipynb", nil)
		if err != nil {
			return nil, err
		}
		if err := e.Client.WaitLog(ctx, e.A, spA, a.ID, "processes", 2*time.Minute); err != nil {
			return nil, err
		}
		var slowest time.Duration
		notReady := 0
		for range 10 {
			start := time.Now()
			if r, err := e.Client.Do(ctx, nil, "GET", "/readyz", nil, nil); err != nil || r.Code != http.StatusOK {
				notReady++
			}
			slowest = max(slowest, time.Since(start))
			_ = apiclient.Sleep(ctx, 100*time.Millisecond)
		}
		if a, err = e.Client.WaitRun(ctx, e.A, spA, a.ID, e.waitTimeout(3*time.Minute)); err != nil {
			return nil, err
		}
		if b, err = e.Client.WaitRun(ctx, e.B, spB, b.ID, e.waitTimeout(3*time.Minute)); err != nil {
			return nil, err
		}
		// On Kubernetes the limit is per Pod (kubelet podPidsLimit), above the
		// container where the wrapper reads its counters: the run fails on the
		// refused fork itself.
		contained := a.ErrorKind == "resource_pids" ||
			(e.Kubernetes && a.ErrorKind == "execution_error" && strings.Contains(a.Message, "Resource temporarily unavailable"))
		ok := a.Status == "failed" && contained && b.Status == "succeeded" && slowest < time.Second && notReady == 0
		return []report.Finding{c.finding("process-limit", "alice spawns processes until failure",
			"failed/resource_pids (on Kubernetes: failed on the refused fork); bob unaffected; service responsive",
			fmt.Sprintf("alice %s/%s; bob %s; slowest /readyz %v (%d not ready)", a.Status, a.ErrorKind, b.Status,
				slowest.Round(time.Millisecond), notReady), ok)}, nil
	}}

// A fills /tmp.
var tmpCheck = &Check{Name: "tmp-limit", Findings: []string{"tmp-limit"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		a, b, err := e.alongside(ctx, "tmpfill.ipynb", nil, "smoke.ipynb")
		if err != nil {
			return nil, err
		}
		ok := a.Status == "failed" && a.ErrorKind == "resource_tmp" && b.Status == "succeeded"
		return []report.Finding{c.finding("tmp-limit", "alice fills /tmp", "failed/resource_tmp; others unaffected",
			fmt.Sprintf("alice %s/%s; bob %s", a.Status, a.ErrorKind, b.Status), ok)}, nil
	}}

// A prints without end.
var endlessCheck = &Check{Name: "endless-output", Findings: []string{"endless-output"},
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		sp, err := e.space(ctx, e.A, map[string][]byte{"endless_print.ipynb": workload("endless_print.ipynb")})
		if err != nil {
			return nil, err
		}
		run, err := e.Client.StartRun(ctx, e.A, sp, "endless_print.ipynb", map[string]any{"seconds": 25})
		if err != nil {
			return nil, err
		}
		// The platform's own log storage for the container, near the end of the run.
		_ = apiclient.Sleep(ctx, 20*time.Second)
		platformLog, measured := -1, false
		if e.PlatformLogBytes != nil {
			platformLog, measured = e.PlatformLogBytes(ctx, run.ID)
		}
		if run, err = e.Client.WaitRun(ctx, e.A, sp, run.ID, e.waitTimeout(3*time.Minute)); err != nil {
			return nil, err
		}
		logBody, _ := e.Client.Log(ctx, e.A, sp, run.ID)
		lines := strings.Split(strings.TrimSuffix(logBody, "\n"), "\n")
		last := lines[len(lines)-1]
		capBytes := int(e.LogBytes)
		if capBytes <= 0 {
			capBytes = 10 << 20
		}
		truncated := run.Result != nil && run.Result.LogTruncated
		nb, _ := e.Client.Do(ctx, e.A, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/output.ipynb", nil, nil)
		platformOK := !measured || (platformLog >= 0 && platformLog <= capBytes+capBytes/10)
		platform := "not measured here (kubelet or the host rotates container logs)"
		if measured {
			platform = fmt.Sprintf("%d bytes", platformLog)
		}
		ok := run.Status == "succeeded" && truncated && strings.HasPrefix(last, "[nbsandbox] log truncated") &&
			len(logBody) <= capBytes+200 && platformOK &&
			len(nb.Body) < 8<<20 && strings.Contains(string(nb.Body), "is not stored in this notebook")
		return []report.Finding{c.finding("endless-output", "alice prints endlessly (200-byte lines for 25 s)",
			"log stops at the cap with a notice; Docker's own log within its cap; host disk unaffected",
			fmt.Sprintf("%s; log.txt %d bytes (cap %d), last line %q; Docker's log %s; output.ipynb %d bytes (stored output capped at 5 MiB)",
				run.Status, len(logBody), capBytes, last[:min(len(last), 40)], platform, len(nb.Body)), ok)}, nil
	}}

// An infinite loop hits the run timeout.
var timeLimitCheck = &Check{Name: "time-limit", Findings: []string{"time-limit"}, Slow: true,
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		sp, err := e.space(ctx, e.A, map[string][]byte{"infinite.ipynb": workload("infinite.ipynb")})
		if err != nil {
			return nil, err
		}
		start := time.Now()
		run, err := e.Client.StartRun(ctx, e.A, sp, "infinite.ipynb", nil)
		if err != nil {
			return nil, err
		}
		if run, err = e.Client.WaitRun(ctx, e.A, sp, run.ID, e.RunTimeout+2*time.Minute); err != nil {
			return nil, err
		}
		took := time.Since(start)
		left, asked := e.containersLeft(ctx, run.ID)
		partial := run.Result != nil && run.Result.Partial
		gone := "not checked (no access to the platform)"
		if asked {
			gone = fmt.Sprintf("%v", left)
		}
		ok := run.Status == "timeout" && partial && len(left) == 0
		return []report.Finding{c.finding("time-limit", "alice runs an infinite loop", "timeout; container removed; partial output kept",
			fmt.Sprintf("%s after %.0fs; partial %v; containers left %s", run.Status, took.Seconds(), partial, gone), ok)}, nil
	}}
