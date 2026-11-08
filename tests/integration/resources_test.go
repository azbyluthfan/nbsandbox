// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runAlongside starts alice's `bad` notebook and bob's `good` one at the same
// time and waits for both.
func runAlongside(t *testing.T, bad string, badParams map[string]any, good string) (Run, Run, string, string) {
	t.Helper()
	spA, spB := space(t, "alice"), space(t, "bob")
	upload(t, "alice", spA, "", map[string]string{bad: ""})
	upload(t, "bob", spB, "", map[string]string{good: ""})
	a := startRun(t, "alice", spA, bad, badParams)
	b := startRun(t, "bob", spB, good, nil)
	return waitRun(t, "alice", spA, a.ID, 3*time.Minute), waitRun(t, "bob", spB, b.ID, 3*time.Minute), spA, spB
}

// Alice allocates past 2 GB while bob runs normally.
func TestMemoryLimit(t *testing.T) {
	a, b, _, _ := runAlongside(t, "oom.ipynb", nil, "smoke.ipynb")
	var peak int64
	if a.Result != nil && a.Result.Usage.PeakMemoryBytes != nil {
		peak = *a.Result.Usage.PeakMemoryBytes
	}
	// On Kubernetes (cgroup v2, 1.28+) an OOM kill takes the whole
	// container, wrapper included, so the peak isn't reported.
	ok := a.Status == "oom" && (peak > 0 || onKubernetes) && b.Status == "succeeded"
	finding(t, "memory-limit", "alice allocates beyond the memory limit while bob runs a normal notebook",
		"alice oom (not kernel_died/failed) with peak memory reported (not on Kubernetes, which kills the whole container); bob succeeded",
		fmt.Sprintf("alice %s (%s), peak %d MiB; bob %s", a.Status, a.Message, peak>>20, b.Status), ok)
	if !ok {
		t.Fatalf("alice %+v bob %+v", a, b)
	}
}

var wallRE = regexp.MustCompile(`cpu_fixed wall=([0-9.]+)s`)

// cpuFixed runs the fixed CPU workload in a space and returns its wall time.
func cpuFixed(t *testing.T, user, sp string) float64 {
	t.Helper()
	run := waitRun(t, user, sp, startRun(t, user, sp, "cpu_fixed.ipynb", nil).ID, 3*time.Minute)
	m := wallRE.FindStringSubmatch(string(call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", user, nil, nil).Body))
	if run.Status != "succeeded" || m == nil {
		t.Fatalf("cpu_fixed: %s", run.Status)
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// Alice spins every core while bob runs fixed CPU work.
// The tolerance (1.5×) gates only when NBSANDBOX_CPU_FAIRNESS_GATE=1 (the
// Linux reference host); elsewhere the ratio is recorded.
func TestCPUFairness(t *testing.T) {
	spB := space(t, "bob")
	upload(t, "bob", spB, "", map[string]string{"cpu_fixed.ipynb": ""})
	var alone []float64
	for i := 0; i < 3; i++ {
		alone = append(alone, cpuFixed(t, "bob", spB))
	}

	spA := space(t, "alice")
	upload(t, "alice", spA, "", map[string]string{"cpu_hog.ipynb": ""})
	hog := startRun(t, "alice", spA, "cpu_hog.ipynb", map[string]any{"seconds": 50})
	waitLog(t, "alice", spA, hog.ID, "Executing Cell 2", time.Minute)
	time.Sleep(2 * time.Second) // let the hog spin up
	var contended []float64
	for i := 0; i < 3; i++ {
		contended = append(contended, cpuFixed(t, "bob", spB))
	}
	hogStill := getRun(t, "alice", spA, hog.ID).Status == "running"
	call(t, "POST", "/v1/spaces/"+spA+"/runs/"+hog.ID+"/cancel", "alice", nil, nil)
	waitRun(t, "alice", spA, hog.ID, time.Minute)

	ratio := median(contended) / median(alone)
	gate := os.Getenv("NBSANDBOX_CPU_FAIRNESS_GATE") == "1"
	ok := ratio <= 1.5 && hogStill
	mode := "report-only on this platform"
	if gate {
		mode = "gating"
	}
	finding(t, "cpu-fairness", "alice spins all cores while bob runs a fixed CPU workload",
		"bob within 1.5× of his solo baseline",
		fmt.Sprintf("bob alone %.2fs, with the hog %.2fs: %.2f× (runs alone %v, contended %v; %s)",
			median(alone), median(contended), ratio, alone, contended, mode), ok || !gate)
	if gate && !ok {
		t.Fatalf("ratio %.2f (hog running throughout: %v)", ratio, hogStill)
	}
	t.Logf("cpu-fairness ratio %.2f (alone %v, contended %v)", ratio, alone, contended)
}

// Alice starts processes until the limit refuses.
func TestProcessLimit(t *testing.T) {
	spA, spB := space(t, "alice"), space(t, "bob")
	upload(t, "alice", spA, "", map[string]string{"pids.ipynb": ""})
	upload(t, "bob", spB, "", map[string]string{"smoke.ipynb": ""})
	a := startRun(t, "alice", spA, "pids.ipynb", nil)
	b := startRun(t, "bob", spB, "smoke.ipynb", nil)
	waitLog(t, "alice", spA, a.ID, "processes", time.Minute)
	var slowest time.Duration
	for i := 0; i < 10; i++ {
		start := time.Now()
		if c := call(t, "GET", "/readyz", "", nil, nil).Code; c != 200 {
			t.Errorf("readyz %d while alice forks", c)
		}
		slowest = max(slowest, time.Since(start))
		time.Sleep(100 * time.Millisecond)
	}
	a = waitRun(t, "alice", spA, a.ID, 3*time.Minute)
	b = waitRun(t, "bob", spB, b.ID, 3*time.Minute)
	// On Kubernetes the limit is per Pod (kubelet podPidsLimit), above the
	// container where the wrapper reads its counters: the run fails on the
	// refused fork itself.
	contained := a.ErrorKind == "resource_pids" ||
		(onKubernetes && a.ErrorKind == "execution_error" && strings.Contains(a.Message, "Resource temporarily unavailable"))
	ok := a.Status == "failed" && contained && b.Status == "succeeded" && slowest < time.Second
	finding(t, "process-limit", "alice spawns processes until failure",
		"failed/resource_pids (on Kubernetes: failed on the refused fork); bob unaffected; service responsive",
		fmt.Sprintf("alice %s/%s; bob %s; slowest /readyz %v", a.Status, a.ErrorKind, b.Status, slowest.Round(time.Millisecond)), ok)
	if !ok {
		t.Fatalf("alice %+v bob %+v", a, b)
	}
}

// An infinite loop hits the run timeout (60 s in the
// test stack, `make test-integration`).
func TestTimeout(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"infinite.ipynb": ""})
	start := time.Now()
	run := waitRun(t, "alice", sp, startRun(t, "alice", sp, "infinite.ipynb", nil).ID, runTimeout+2*time.Minute)
	took := time.Since(start)
	left := runContainers(t)
	ok := run.Status == "timeout" && run.Result != nil && run.Result.Partial && len(left) == 0
	finding(t, "time-limit", "alice runs an infinite loop", "timeout; container removed; partial output kept",
		fmt.Sprintf("%s after %.0fs; partial %v; containers left %v", run.Status, took.Seconds(), run.Result != nil && run.Result.Partial, left), ok)
	if !ok {
		t.Fatalf("run %+v left %v", run, left)
	}
}

// Alice fills /tmp.
func TestTmpFull(t *testing.T) {
	a, b, _, _ := runAlongside(t, "tmpfill.ipynb", nil, "smoke.ipynb")
	ok := a.Status == "failed" && a.ErrorKind == "resource_tmp" && b.Status == "succeeded"
	finding(t, "tmp-limit", "alice fills /tmp", "failed/resource_tmp; others unaffected",
		fmt.Sprintf("alice %s/%s; bob %s", a.Status, a.ErrorKind, b.Status), ok)
	if !ok {
		t.Fatalf("alice %+v bob %+v", a, b)
	}
}

// Alice prints without end.
func TestEndlessOutput(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"endless_print.ipynb": ""})
	run := startRun(t, "alice", sp, "endless_print.ipynb", map[string]any{"seconds": 25})
	// Docker's own log storage for the container, near the end of the run.
	time.Sleep(20 * time.Second)
	dockerLog := -1
	if onKubernetes {
		dockerLog = 0 // kubelet rotates container logs itself; nothing of ours to measure
	} else if out, err := exec.Command("sh", "-c", "docker logs nbsandbox-run-"+run.ID+" 2>&1 | wc -c").Output(); err == nil {
		dockerLog, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	run = waitRun(t, "alice", sp, run.ID, 3*time.Minute)
	logBody := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", "alice", nil, nil).Body
	lines := strings.Split(strings.TrimSuffix(string(logBody), "\n"), "\n")
	last := lines[len(lines)-1]
	const capBytes = 10 << 20
	var truncated bool
	if run.Result != nil {
		truncated = strings.Contains(string(call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID, "alice", nil, nil).Body), `"log_truncated":true`)
	}
	nb := call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/output.ipynb", "alice", nil, nil).Body
	ok := run.Status == "succeeded" && truncated && strings.HasPrefix(last, "[nbsandbox] log truncated") &&
		len(logBody) <= capBytes+200 && dockerLog >= 0 && dockerLog <= capBytes+capBytes/10 &&
		len(nb) < 8<<20 && strings.Contains(string(nb), "is not stored in this notebook")
	finding(t, "endless-output", "alice prints endlessly (200-byte lines for 25 s)",
		"log stops at the cap with a notice; Docker's own log within its cap; host disk unaffected",
		fmt.Sprintf("%s; log.txt %d bytes (cap %d), last line %q; Docker's log %d bytes; output.ipynb %d bytes (stored output capped at 5 MiB)",
			run.Status, len(logBody), capBytes, last[:min(len(last), 40)], dockerLog, len(nb)), ok)
	if !ok {
		t.Fatalf("status %s truncated %v log %d docker %d last %q", run.Status, truncated, len(logBody), dockerLog, last)
	}
}
