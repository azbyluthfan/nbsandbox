// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/packages"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

const humanizeLock = "humanize==4.17.0 --hash=sha256:02d0899527760efe18011944f882df4970943b4cdff7b5a0ba7fed43a576ef98\n"

// pkgBuilder builds like build_packages.py: humanize, or a failure when
// asked for badpkg.
type pkgBuilder struct{}

func (pkgBuilder) BuildPackages(_ context.Context, spec isolation.BuildSpec, out isolation.LineSink) (isolation.RunResult, error) {
	b, _ := os.ReadFile(filepath.Join(spec.InDir, "request.json"))
	var req struct{ Requirements string }
	_ = json.Unmarshal(b, &req)
	out.Line("== installing from requirements.txt")
	result := map[string]any{"ok": true, "lock": humanizeLock, "modules": []string{"humanize"}}
	if strings.Contains(req.Requirements, "badpkg") {
		out.Line("ERROR: No matching distribution found for badpkg")
		result = map[string]any{"ok": false, "error": "pip couldn't install the requirements"}
	} else {
		_ = os.MkdirAll(filepath.Join(spec.OutDir, "site-packages", "humanize"), 0o755)
		_ = os.WriteFile(filepath.Join(spec.OutDir, "site-packages", "humanize", "__init__.py"), []byte("x = 1\n"), 0o644)
	}
	rb, _ := json.Marshal(result)
	_ = os.WriteFile(filepath.Join(spec.OutDir, "result.json"), rb, 0o644)
	return isolation.RunResult{}, nil
}

func (pkgBuilder) RemoveBuildOrphans(context.Context, string) error { return nil }

// packagesEnv has package sets on; start runs the build loop.
func packagesEnv(t *testing.T) (testEnv, func()) {
	t.Helper()
	var svc *packages.Service
	e := newEnvFull(t, nil, func(db *store.DB, data string) *packages.Service {
		_ = os.MkdirAll(filepath.Join(data, "tmp"), 0o700)
		svc = packages.New(packages.Config{Enabled: true, IndexURL: "https://pypi.org/simple", MaxSize: 1 << 20, BuildTimeout: time.Minute,
			Retention: 24 * time.Hour, CacheSize: 10 << 20, BuilderImage: "builder:test", Network: "bridge", DataDir: data, WorkerID: "w1",
			Platform: "linux/amd64"}, db, blob.NewMemory(), pkgBuilder{}, nil)
		return svc
	})
	start := func() {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go svc.Run(ctx)
	}
	return e, start
}

func (e testEnv) packages(t *testing.T, user, spaceID string) runs.PackagesView {
	t.Helper()
	rec := e.req("GET", "/v1/spaces/"+spaceID+"/packages", user, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("packages: %d %s", rec.Code, rec.Body)
	}
	var v runs.PackagesView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return v
}

// A run waits for its space's packages, then runs with them mounted; a
// published version keeps them.
func TestRunsWithPackages(t *testing.T) {
	e, startBuilds := packagesEnv(t)
	sp := e.createSpace(t, "alice", "")
	if v := e.packages(t, "alice", sp.ID); !v.Enabled || v.Status != "none" {
		t.Fatalf("no requirements yet: %+v", v)
	}
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON, "requirements.txt", "humanize\n")
	if v := e.packages(t, "alice", sp.ID); v.Status != "not_built" {
		t.Fatalf("before any build: %+v", v)
	}

	// No builder yet: the run waits, queued, and says why.
	run := e.startRun(t, sp.ID, "nb.ipynb")
	if run.Status != runs.StatusQueued || run.WaitingFor != "packages" {
		t.Fatalf("waiting: %+v", run)
	}
	time.Sleep(200 * time.Millisecond)
	var now runs.View
	_ = json.Unmarshal(e.req("GET", "/v1/spaces/"+sp.ID+"/runs/"+run.ID, "alice", nil, "").Body.Bytes(), &now)
	if now.Status != runs.StatusQueued || now.WaitingFor != "packages" {
		t.Fatalf("started without its packages: %+v", now)
	}

	startBuilds()
	done := e.waitRun(t, "alice", sp.ID, run.ID)
	if done.Status != runs.StatusSucceeded {
		t.Fatalf("with packages: %+v", done)
	}
	e.backend.mu.Lock()
	dir := e.backend.packagesDirs[len(e.backend.packagesDirs)-1]
	e.backend.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "humanize", "__init__.py")); dir == "" || err != nil {
		t.Fatalf("the set wasn't mounted: %q %v", dir, err)
	}
	v := e.packages(t, "alice", sp.ID)
	if v.Status != "ready" || len(v.Installed) != 1 || v.Installed[0].Name != "humanize" || v.Installed[0].Version != "4.17.0" ||
		v.BuiltAt == nil || !strings.Contains(v.BuildLog, "installing") {
		t.Fatalf("ready: %+v", v)
	}
	// A run of the same requirements doesn't wait.
	again := e.startRun(t, sp.ID, "nb.ipynb")
	if again.WaitingFor != "" {
		t.Errorf("waited for a ready set: %+v", again)
	}
	e.waitRun(t, "alice", sp.ID, again.ID)

	// Publish keeps the set; an invocation runs with it.
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/functions/f/versions", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}
	rec := e.req("POST", "/v1/spaces/"+sp.ID+"/functions/f/invoke", "alice", strings.NewReader(`{"parameters":{}}`), "application/json")
	var inv runs.View
	_ = json.Unmarshal(rec.Body.Bytes(), &inv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invoke: %d %s", rec.Code, rec.Body)
	}
	if done := e.waitRun(t, "alice", sp.ID, inv.ID); done.Status != runs.StatusSucceeded {
		t.Fatalf("invocation: %+v", done)
	}
	e.backend.mu.Lock()
	last := e.backend.packagesDirs[len(e.backend.packagesDirs)-1]
	e.backend.mu.Unlock()
	if last == "" || last == dir {
		t.Errorf("the invocation runs the version's own (locked) set: %q (space's: %q)", last, dir)
	}
}

func TestPackagesFailures(t *testing.T) {
	e, startBuilds := packagesEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON, "requirements.txt", "-i http://169.254.169.254/\n")
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/runs", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("index option: %d %s", rec.Code, rec.Body)
	}
	if v := e.packages(t, "alice", sp.ID); v.Status != "failed" || !strings.Contains(v.Error, "not accepted") {
		t.Errorf("shown refused: %+v", v)
	}

	// A build that fails fails the runs waiting for it; the next run is
	// refused at once, with the build log.
	e.upload(t, "alice", sp.ID, "", "requirements.txt", "badpkg\n")
	waiting := e.startRun(t, sp.ID, "nb.ipynb")
	startBuilds()
	done := e.waitRun(t, "alice", sp.ID, waiting.ID)
	if done.Status != runs.StatusFailed || done.ErrorKind != runs.KindPackages || done.Fault != "caller" || !strings.Contains(done.Message, "pip couldn't") {
		t.Fatalf("waiting run: %+v", done)
	}
	rec := e.req("POST", "/v1/spaces/"+sp.ID+"/runs", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"packages_failed"`) || !strings.Contains(rec.Body.String(), "badpkg") {
		t.Fatalf("next run: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/functions/f/versions", "alice", strings.NewReader(`{"notebook":"nb.ipynb"}`), "application/json"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("publish with failed packages: %d %s", rec.Code, rec.Body)
	}
	// Another user can't read or build alice's packages; building needs a
	// requirements.txt.
	if rec := e.req("GET", "/v1/spaces/"+sp.ID+"/packages", "bob", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("bob: %d", rec.Code)
	}
	other := e.createSpace(t, "alice", "")
	if rec := e.req("POST", "/v1/spaces/"+other.ID+"/packages", "alice", nil, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("build without requirements.txt: %d", rec.Code)
	}
}

func TestBuildPackagesAhead(t *testing.T) {
	e, startBuilds := packagesEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "requirements.txt", "humanize\n")
	rec := e.req("POST", "/v1/spaces/"+sp.ID+"/packages", "alice", nil, "")
	var v runs.PackagesView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusAccepted || v.Status != "pending" {
		t.Fatalf("build: %d %s", rec.Code, rec.Body)
	}
	startBuilds()
	deadline := time.Now().Add(5 * time.Second)
	for e.packages(t, "alice", sp.ID).Status != "ready" {
		if time.Now().After(deadline) {
			t.Fatal("not built")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/packages", "alice", nil, ""); rec.Code != http.StatusOK {
		t.Errorf("already built: %d", rec.Code)
	}
}

// With package sets off, nothing changes: requirements.txt is only checked.
func TestPackagesDisabled(t *testing.T) {
	e := newEnv(t)
	sp := e.createSpace(t, "alice", "")
	e.upload(t, "alice", sp.ID, "", "nb.ipynb", nbJSON, "requirements.txt", "humanize\n")
	if v := e.packages(t, "alice", sp.ID); v.Enabled || v.Status != "disabled" {
		t.Errorf("off: %+v", v)
	}
	if rec := e.req("POST", "/v1/spaces/"+sp.ID+"/packages", "alice", nil, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("build while off: %d", rec.Code)
	}
	if v := e.runToEnd(t, "alice", sp.ID, `{"notebook":"nb.ipynb"}`); v.Status != runs.StatusSucceeded || v.WaitingFor != "" {
		t.Errorf("run: %+v", v)
	}
}
