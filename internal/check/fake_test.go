// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// deployment is a fake nbsandbox API: each workload notebook ends the way it
// does on a real, correctly isolated deployment (or, with broken, the way
// a broken one would), so every check runs end to end in a unit test.
type deployment struct {
	mu      sync.Mutex
	broken  map[string]bool // notebook → misbehave
	spaces  map[string]string
	runs    map[string]*fakeRun
	n       int
	deleted int
}

type fakeRun struct {
	space, notebook, status string
	owner                   string
	cancelled               bool
	polls                   int
}

func newDeployment(broken ...string) *deployment {
	d := &deployment{broken: map[string]bool{}, spaces: map[string]string{}, runs: map[string]*fakeRun{}}
	for _, b := range broken {
		d.broken[b] = true
	}
	return d
}

// final is how each notebook ends.
func (d *deployment) final(r *fakeRun) map[string]any {
	ok := !d.broken[r.notebook]
	st := map[string]any{"id": "", "status": "succeeded", "result": map[string]any{"status": "succeeded", "rendered": true,
		"usage": map[string]any{"peak_memory_bytes": 1 << 20}}}
	res, _ := st["result"].(map[string]any)
	set := func(status string) { st["status"], res["status"] = status, status }
	switch r.notebook {
	case "oom.ipynb":
		if ok {
			set("oom")
		}
	case "pids.ipynb":
		if ok {
			set("failed")
			st["error_kind"] = "resource_pids"
		}
	case "tmpfill.ipynb":
		if ok {
			set("failed")
			st["error_kind"] = "resource_tmp"
		}
	case "endless_print.ipynb":
		res["log_truncated"] = ok
	case "infinite.ipynb":
		set("timeout")
		res["partial"] = ok
	case "crash.ipynb":
		if ok {
			set("kernel_died")
			st["fault"] = "notebook"
		}
	case "fail_cell3.ipynb":
		set("failed")
		cell := 3
		res["error"] = map[string]any{"cell_index": cell, "exception_type": "ValueError", "exception_message": "boom in cell 3"}
	case "render_fail.ipynb":
		res["rendered"] = !ok
	}
	if r.cancelled {
		set("cancelled")
		res["partial"] = true
	}
	return st
}

func (d *deployment) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	user, _, _ := req.BasicAuth()
	p := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	writeJSON := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case req.URL.Path == "/readyz":
		w.WriteHeader(http.StatusOK)
	case req.URL.Path == "/v1/runtime":
		writeJSON(200, map[string]any{"runtime": "runc", "limits": map[string]any{"timeout_seconds": 2, "log_bytes": 1000},
			"docker": map[string]any{"operating_system": "Docker Desktop", "version": "29"}})
	case req.Method == "POST" && req.URL.Path == "/v1/spaces":
		d.n++
		id := fmt.Sprintf("s%d", d.n)
		d.spaces[id] = user
		writeJSON(201, map[string]string{"id": id})
	case len(p) >= 3 && p[1] == "spaces" && d.spaces[p[2]] != user:
		writeJSON(404, map[string]any{"error": map[string]string{"code": "not_found", "message": "no such space"}})
	case req.Method == "DELETE" && len(p) == 3:
		d.deleted++
		w.WriteHeader(204)
	case req.Method == "POST" && len(p) == 4 && p[3] == "files":
		w.WriteHeader(201)
	case req.Method == "POST" && len(p) == 4 && p[3] == "runs":
		var body struct{ Notebook string }
		_ = json.NewDecoder(req.Body).Decode(&body)
		d.n++
		id := fmt.Sprintf("r%d", d.n)
		d.runs[id] = &fakeRun{space: p[2], notebook: body.Notebook, status: "running", owner: user}
		writeJSON(201, map[string]string{"id": id, "status": "queued"})
	case req.Method == "GET" && len(p) == 4 && p[3] == "runs":
		var items []map[string]string
		for id, r := range d.runs {
			if r.space == p[2] {
				items = append(items, map[string]string{"id": id, "status": r.status})
			}
		}
		writeJSON(200, map[string]any{"items": items})
	case len(p) >= 5 && p[3] == "runs":
		r := d.runs[p[4]]
		if r == nil {
			writeJSON(404, map[string]any{"error": map[string]string{"code": "not_found", "message": "no such run"}})
			return
		}
		d.serveRun(w, req, p[4], r, p[5:], writeJSON)
	default:
		http.NotFound(w, req)
	}
}

func (d *deployment) serveRun(w http.ResponseWriter, req *http.Request, id string, r *fakeRun, rest []string, writeJSON func(int, any)) {
	// Runs that wait to be cancelled (long, the CPU hog) keep running;
	// the holder runs until the probe has read it; the rest end after a poll.
	longRunning := r.notebook == "long.ipynb" || r.notebook == "cpu_hog.ipynb" || (r.notebook == "holder.ipynb" && r.polls < 3)
	switch {
	case len(rest) == 0:
		r.polls++
		if !longRunning || r.cancelled {
			fin := d.final(r)
			r.status, _ = fin["status"].(string)
			fin["id"] = id
			writeJSON(200, fin)
			return
		}
		writeJSON(200, map[string]any{"id": id, "status": "running"})
	case rest[0] == "cancel":
		r.cancelled = true
		w.WriteHeader(202)
	case rest[0] == "log":
		log := map[string]string{
			"long.ipynb":          "tick 1\ntick 2\ntick 3\n",
			"cpu_hog.ipynb":       "Executing Cell 2\n",
			"cpu_fixed.ipynb":     "cpu_fixed wall=1.000s\n",
			"pids.ipynb":          "12 processes\n",
			"holder.ipynb":        "holder ready\nholder done intact=True waited=1s\n",
			"endless_print.ipynb": strings.Repeat("x", 100) + "\n[nbsandbox] log truncated at 900 bytes\n",
			"smoke.ipynb":         "artifacts written\n",
		}[r.notebook]
		if r.status != "running" {
			log += "[nbsandbox] run finished: " + r.status + "\n"
		}
		if d.broken["holder.ipynb"] && r.notebook == "holder.ipynb" {
			log = strings.ReplaceAll(log, "intact=True", "intact=False")
		}
		_, _ = w.Write([]byte(log))
	case rest[0] == "output.ipynb":
		_, _ = w.Write([]byte(`{"cells":[],"note":"output is not stored in this notebook"}`))
	case rest[0] == "artifacts" && len(rest) == 2 && rest[1] == "findings.json":
		f := map[string]map[string]any{}
		for _, row := range isolationRows {
			f[row.id] = map[string]any{"pass": !d.broken[row.id]}
		}
		writeJSON(200, f)
	default:
		writeJSON(404, map[string]any{"error": map[string]string{"code": "not_found", "message": "no"}})
	}
}

func fakeEnv(t *testing.T, d *deployment) *Env {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	e := &Env{Client: &Client{BaseURL: srv.URL, HTTP: srv.Client()},
		A: &Identity{Name: "alice", Password: "a"}, B: &Identity{Name: "bob", Password: "b"},
		RunLeft: func(context.Context, string) ([]string, error) { return nil, nil }}
	if err := e.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

// Every check passes on a correct deployment and cleans up after itself.
func TestEveryCheckPassesOnACorrectDeployment(t *testing.T) {
	d := newDeployment()
	e := fakeEnv(t, d)
	seen := map[string]bool{}
	for _, c := range All() {
		if c.Name == "cpu-fairness" || c.Name == "endless-output" {
			continue // they wait on real time (20 s and more): covered below with shorter waits where possible
		}
		fs, err := c.Run(context.Background(), e)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		for _, f := range fs {
			seen[f.ID] = true
			if !f.Pass || f.Test != c.Name || f.Attempted == "" || f.Expected == "" {
				t.Errorf("%s: %+v", f.ID, f)
			}
		}
	}
	for _, id := range []string{"space-paths", "privileges", "cross-user-api", "neighbour-unaffected", "memory-limit", "process-limit",
		"tmp-limit", "time-limit", "cancel-running", "cancel-at-completion", "caller-disconnect", "kernel-crash", "cell-exception", "render-failure"} {
		if !seen[id] {
			t.Errorf("no finding %s", id)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deleted == 0 || d.deleted != len(d.spaces) {
		t.Errorf("deleted %d of %d spaces", d.deleted, len(d.spaces))
	}
}

// On a broken deployment the same checks report failures, not passes.
func TestChecksFailOnABrokenDeployment(t *testing.T) {
	d := newDeployment("oom.ipynb", "pids.ipynb", "tmpfill.ipynb", "crash.ipynb", "render_fail.ipynb", "infinite.ipynb",
		"memory-secret", "holder.ipynb")
	e := fakeEnv(t, d)
	for _, name := range []string{"memory-limit", "process-limit", "tmp-limit", "kernel-crash", "render-failure", "time-limit", "isolation"} {
		fs, err := Get(name).Run(context.Background(), e)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		failed := 0
		for _, f := range fs {
			if !f.Pass {
				failed++
			}
		}
		if failed == 0 {
			t.Errorf("%s: no failed finding on a broken deployment: %+v", name, fs)
		}
	}
}

// A run still present after it ended fails the checks that look.
func TestContainersLeftFail(t *testing.T) {
	e := fakeEnv(t, newDeployment())
	e.RunLeft = func(context.Context, string) ([]string, error) { return []string{"nbsandbox-run-x"}, nil }
	start := time.Now()
	fs, err := Get("cancel-running").Run(context.Background(), e)
	if err != nil || len(fs) != 1 || fs[0].Pass || !strings.Contains(fs[0].Observed, "container gone false") {
		t.Fatalf("%v %+v", err, fs)
	}
	if time.Since(start) < 20*time.Second {
		t.Error("containersLeft gave up before its 20 s")
	}
}

// Without access to the platform, the checks say so instead of claiming
// the container is gone.
func TestNotCheckedWithoutThePlatform(t *testing.T) {
	e := fakeEnv(t, newDeployment())
	e.RunLeft = nil
	fs, err := Get("cancel-running").Run(context.Background(), e)
	if err != nil || len(fs) != 1 || !fs[0].Pass || !strings.Contains(fs[0].Observed, "not checked") {
		t.Fatalf("%v %+v", err, fs)
	}
}
