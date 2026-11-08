// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// metric reads one unlabelled value from /metrics.
func metric(t *testing.T, name string) float64 {
	t.Helper()
	v, err := readMetric(name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func readMetric(name string) (float64, error) {
	resp, err := httpClient.Get(baseURL + "/metrics")
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, name+" ") {
			return strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name)), 64)
		}
	}
	return -1, sc.Err()
}

// soakEnv reads a duration setting of the soak.
func soakEnv(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

// sample is one minute's reading of the things that leak.
type sample struct {
	at                       time.Duration // since the baseline
	procs                    map[string]procSample
	containers, dataMiB, run float64
}

// procSample is one nbsandboxd process's figures.
type procSample struct{ goroutines, fds, rssMiB float64 }

// metricSource is one nbsandboxd process whose /metrics the soak samples.
type metricSource struct {
	name  string
	fetch func() (string, error)
}

// metricSources lists every nbsandboxd process: one on the single-node
// stack; each API process and worker on the cluster stacks, sampled
// separately (a load balancer would hand each sample to a different one).
func metricSources(t *testing.T) []metricSource {
	t.Helper()
	run := func(args ...string) func() (string, error) {
		return func() (string, error) {
			out, err := exec.Command(args[0], args[1:]...).Output()
			return string(out), err
		}
	}
	switch {
	case onKubernetes:
		var out []metricSource
		for _, pod := range strings.Fields(kubectl(t, "get", "pods", "-n", "nbsandbox", "-l", "app.kubernetes.io/component in (api,worker)",
			"-o", "jsonpath={.items[*].metadata.name}")) {
			out = append(out, metricSource{pod, run("docker", "exec", kindNode, "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf",
				"get", "--raw", "/api/v1/namespaces/nbsandbox/pods/"+pod+":8080/proxy/metrics")})
		}
		return out
	case clustered:
		// The load balancer's container has wget and reaches every process.
		var out []metricSource
		for _, svc := range []string{"api", "nbsandboxd"} {
			for n := 1; n <= 2; n++ {
				c := fmt.Sprintf("nbsandbox-cluster-%s-%d", svc, n)
				out = append(out, metricSource{c, run("docker", "exec", "nbsandbox-cluster-lb-1", "wget", "-qO-", "http://"+c+":8080/metrics")})
			}
		}
		return out
	default:
		return []metricSource{{"nbsandboxd", func() (string, error) {
			resp, err := httpClient.Get(baseURL + "/metrics")
			if err != nil {
				return "", err
			}
			defer func() { _ = resp.Body.Close() }()
			var b strings.Builder
			_, err = bufio.NewReader(resp.Body).WriteTo(&b)
			return b.String(), err
		}}}
	}
}

// metricValue finds one unlabelled value in a /metrics page (-1: absent).
func metricValue(page, name string) float64 {
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, name+" ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name)), 64)
			if err == nil {
				return v
			}
		}
	}
	return -1
}

// TestSoak runs mixed workloads in several spaces at once for SOAK_DURATION,
// samples nbsandboxd every SOAK_SAMPLE (default 1m), and fails on steady
// growth: it fits a line to each series rather than comparing two snapshots,
// so a leak shows within an hour. Started by `make soak`, which also
// shortens the cleanup timers (retention, event retention, idempotency
// window, cleanup interval) so every cleanup path runs many times; with
// them on, the data folder must level off too. Skipped unless SOAK_DURATION
// is set. Results are compared with the 13-hour baseline in docs/perf.md.
func TestSoak(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("SOAK_DURATION"))
	if err != nil || d <= 0 {
		t.Skip("set SOAK_DURATION (e.g. 45m; make soak sets it) to run the soak test")
	}
	every := soakEnv(t, "SOAK_SAMPLE", time.Minute)
	retention := soakEnv(t, "NBSANDBOX_RUN_RETENTION", 0)        // as given to the stack; 0 = default (not shortened)
	window := soakEnv(t, "NBSANDBOX_IDEMPOTENCY_WINDOW", 0)      // likewise
	eventRetention := soakEnv(t, "NBSANDBOX_EVENT_RETENTION", 0) // likewise
	nSpaces := 4
	if v, err := strconv.Atoi(os.Getenv("SOAK_SPACES")); err == nil && v > 0 {
		nSpaces = v
	}
	notebooks := []string{"smoke.ipynb", "fail_cell3.ipynb", "stream.ipynb", "crash.ipynb"}
	users := []string{"alice", "bob"}
	spaces := make([]string, nSpaces)
	for i := range spaces {
		spaces[i] = space(t, users[i%2])
		upload(t, users[i%2], spaces[i], "", map[string]string{"smoke.ipynb": "", "fail_cell3.ipynb": "", "stream.ipynb": "", "crash.ipynb": ""})
	}
	// A space for the idempotency check: a key used now must start a new run
	// once the window has passed, instead of returning this one.
	keySpace := space(t, "alice")
	upload(t, "alice", keySpace, "", map[string]string{"smoke.ipynb": ""})
	key := "soak-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	first := jsonCall(t, "POST", "/v1/spaces/"+keySpace+"/runs", "alice", map[string]any{"notebook": "smoke.ipynb"}, map[string]string{"Idempotency-Key": key})
	if first.Code != http.StatusCreated {
		t.Fatalf("idempotent run: %d %s", first.Code, first.Body)
	}
	var firstRun Run
	first.json(t, &firstRun)
	waitRun(t, "alice", keySpace, firstRun.ID, 3*time.Minute)
	keyUsed := time.Now()

	// Warm up: one cycle per space, then the baseline.
	cycle := func(t *testing.T, i, n int) {
		user, sp := users[i%2], spaces[i]
		run := startRun(t, user, sp, notebooks[(n+i)%len(notebooks)], nil)
		if n%5 == 4 { // sometimes watch the live log, then cancel
			s := openStream(t, user, sp, run.ID, 0)
			_, _ = s.next()
			s.close()
			call(t, "POST", "/v1/spaces/"+sp+"/runs/"+run.ID+"/cancel", user, nil, nil)
		}
		waitRun(t, user, sp, run.ID, 3*time.Minute)
	}
	for i := range spaces {
		cycle(t, i, 0)
	}
	dataDir := os.Getenv("NBSANDBOX_DATA_DIR")
	if onKubernetes {
		dataDir = "" // the workers' scratch is in their Pods
	}
	sources := metricSources(t)
	if len(sources) == 0 {
		t.Fatal("no nbsandboxd processes to sample")
	}
	start := time.Now()
	var (
		mu      sync.Mutex
		samples []sample
	)
	take := func() {
		s := sample{at: time.Since(start), procs: map[string]procSample{}}
		for _, src := range sources {
			page, err := src.fetch()
			if err != nil {
				continue // a missed sample; the fit copes
			}
			s.procs[src.name] = procSample{metricValue(page, "go_goroutines"), metricValue(page, "process_open_fds"),
				metricValue(page, "process_resident_memory_bytes") / (1 << 20)}
			if v := metricValue(page, "nbsandbox_active_runs"); v > 0 {
				s.run += v
			}
		}
		s.containers = float64(len(runContainersQuiet()))
		s.dataMiB = float64(dirBytes(dataDir)) / (1 << 20)
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	}
	take()
	ctx, stop := context.WithCancel(context.Background())
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				take()
			}
		}
	}()

	// The workload: every space busy at once until the time is up.
	var cycles atomic.Int64
	end := start.Add(d)
	t.Run("workers", func(t *testing.T) {
		for i := range spaces {
			t.Run(fmt.Sprintf("space-%d", i), func(t *testing.T) {
				t.Parallel()
				for n := 1; time.Now().Before(end); n++ {
					cycle(t, i, n)
					cycles.Add(1)
				}
			})
		}
	})
	stop()
	<-samplerDone
	time.Sleep(3 * time.Second) // let the last containers go
	take()

	// Cleanup worked: no finished run older than retention plus two cleanup
	// intervals is left, events were pruned, and the idempotency key expired.
	var checks []string
	cleanupOK := true
	if retention > 0 {
		var list struct {
			Items []struct {
				FinishedAt *time.Time `json:"finished_at"`
			}
		}
		call(t, "GET", "/v1/spaces/"+spaces[0]+"/runs?limit=200", users[0], nil, nil).json(t, &list)
		limit := retention + 2*soakEnv(t, "NBSANDBOX_CLEANUP_EVERY", time.Hour) + time.Minute
		var oldest time.Duration
		for _, r := range list.Items {
			if r.FinishedAt != nil && time.Since(*r.FinishedAt) > oldest {
				oldest = time.Since(*r.FinishedAt)
			}
		}
		ok := oldest <= limit
		cleanupOK = cleanupOK && ok
		checks = append(checks, fmt.Sprintf("oldest finished run %s old (limit %s): %v", oldest.Round(time.Second), limit, ok))
	}
	if eventRetention > 0 && d > eventRetention+5*time.Minute {
		ok := firstEventName(t, "alice", keySpace, 1) == "pruned"
		cleanupOK = cleanupOK && ok
		checks = append(checks, fmt.Sprintf("resuming from event 1 after the event retention says pruned: %v", ok))
	}
	if window > 0 && time.Since(keyUsed) > window+5*time.Minute {
		again := jsonCall(t, "POST", "/v1/spaces/"+keySpace+"/runs", "alice", map[string]any{"notebook": "smoke.ipynb", "space": "read-only"}, map[string]string{"Idempotency-Key": key})
		ok := again.Code == http.StatusCreated // within the window it would be 422 (same key, different body)
		cleanupOK = cleanupOK && ok
		checks = append(checks, fmt.Sprintf("the idempotency key expired after the window: %v (%d)", ok, again.Code))
		if ok {
			var r Run
			again.json(t, &r)
			waitRun(t, "alice", keySpace, r.ID, 3*time.Minute)
		}
	}

	// Trends, per hour, over the samples after the first quarter (the stack
	// settles: caches, pools, the first cleanup).
	mu.Lock()
	all := append([]sample(nil), samples...)
	mu.Unlock()
	tail := all[len(all)/4:]
	// Each process on its own; the worst slope of each figure is judged.
	var gr, fd, rss float64
	var grProc, fdProc, rssProc string
	for _, src := range sources {
		var ss []sample
		for _, s := range tail {
			if _, ok := s.procs[src.name]; ok {
				ss = append(ss, s)
			}
		}
		of := func(f func(procSample) float64) float64 {
			return perHour(ss, func(s sample) float64 { return f(s.procs[src.name]) })
		}
		if v := of(func(p procSample) float64 { return p.goroutines }); grProc == "" || v > gr {
			gr, grProc = v, src.name
		}
		if v := of(func(p procSample) float64 { return p.fds }); fdProc == "" || v > fd {
			fd, fdProc = v, src.name
		}
		if v := of(func(p procSample) float64 { return p.rssMiB }); rssProc == "" || v > rss {
			rss, rssProc = v, src.name
		}
	}
	// The data folder can only level off once cleanup removes runs: judge it
	// from retention plus two cleanup intervals on, and only with enough
	// samples after that.
	var data float64
	dataJudged := false
	if retention > 0 {
		from := retention + 2*soakEnv(t, "NBSANDBOX_CLEANUP_EVERY", time.Hour)
		var settled []sample
		for _, s := range all {
			if s.at >= from {
				settled = append(settled, s)
			}
		}
		if len(settled) >= 5 {
			data, dataJudged = perHour(settled, func(s sample) float64 { return s.dataMiB }), true
		}
	}
	last := all[len(all)-1]
	// Limits: growth that would matter over a day, well above sampling noise.
	trendOK := gr <= 10 && fd <= 10 && rss <= 20 && (!dataJudged || data <= 50) && last.containers == 0 &&
		call(t, "GET", "/readyz", "", nil, nil).Code == http.StatusOK
	dataNote := fmt.Sprintf("%+.1f MiB", data)
	switch {
	case dataDir == "":
		dataNote = "not measured (no host data folder; contents are in object storage)"
	case !dataJudged:
		dataNote = "not judged (run longer than retention plus two cleanups)"
	}
	var csv strings.Builder
	csv.WriteString("minutes,process,goroutines,open_fds,rss_mib,run_containers,data_mib,active_runs\n")
	for _, s := range all {
		for _, src := range sources {
			if p, ok := s.procs[src.name]; ok {
				fmt.Fprintf(&csv, "%.1f,%s,%.0f,%.0f,%.1f,%.0f,%.1f,%.0f\n", s.at.Minutes(), src.name, p.goroutines, p.fds, p.rssMiB, s.containers, s.dataMiB, s.run)
			}
		}
	}
	var ends []string
	for _, src := range sources {
		if p, ok := last.procs[src.name]; ok {
			ends = append(ends, fmt.Sprintf("%s %.0f goroutines, %.0f fds, %.0f MiB", src.name, p.goroutines, p.fds, p.rssMiB))
		}
	}
	t.Logf("samples:\n%s", csv.String())
	if dir := os.Getenv("REPORT_DIR"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "soak-samples.csv"), []byte(csv.String()), 0o644)
	}
	runs := cycles.Load()
	finding(t, "SOAK", fmt.Sprintf("%d spaces at once for %s (%d runs), %d processes sampled every %s; cleanup timers %s",
		nSpaces, d, runs, len(sources), every, map[bool]string{true: "shortened", false: "default"}[retention > 0]),
		"no steady growth in goroutines, open files, memory or data; no leftover containers; cleanup removes old runs, events and keys",
		fmt.Sprintf("worst per hour: goroutines %+.1f (%s), open fds %+.1f (%s), RSS %+.1f MiB (%s), data %s; end: %s; %.0f MiB data, %.0f containers; %s",
			gr, grProc, fd, fdProc, rss, rssProc, dataNote, strings.Join(ends, "; "), last.dataMiB, last.containers, strings.Join(checks, "; ")),
		trendOK && cleanupOK)
	if !trendOK || !cleanupOK {
		t.Fatalf("soak failed: trends ok %v, cleanup ok %v (%s)", trendOK, cleanupOK, strings.Join(checks, "; "))
	}
}

// perHour is the least-squares slope of f over the samples, per hour.
func perHour(ss []sample, f func(sample) float64) float64 {
	if len(ss) < 3 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for _, s := range ss {
		x, y := s.at.Hours(), f(s)
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	n := float64(len(ss))
	den := n*sxx - sx*sx
	if math.Abs(den) < 1e-12 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// dirBytes is the size of the files under dir (0 when unset or unreadable).
func dirBytes(dir string) int64 {
	if dir == "" {
		return 0
	}
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// firstEventName opens the space's status stream resuming after an event ID
// and returns the first event's name.
func firstEventName(t *testing.T, user, spaceID string, after int64) string {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/v1/spaces/"+spaceID+"/events", nil)
	req.SetBasicAuth(user, passwords[user])
	req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	s := &stream{resp: resp, r: bufio.NewReader(resp.Body)}
	ev, err := s.next()
	if err != nil {
		return ""
	}
	return ev.Name
}

// runContainersQuiet lists run containers without a *testing.T, for the
// sampler goroutine (nil on error).
func runContainersQuiet() []string {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "label=nbsandbox.run", "--format", "{{.Names}}").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}
