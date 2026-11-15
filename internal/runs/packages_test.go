// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/packages"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

const torchLock = "torch==2.5.0 --hash=sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12" + "\n"

// setBuilder builds like build_packages.py: torch, or a failure for badpkg.
type setBuilder struct {
	mu       sync.Mutex
	requests []string
}

func (b *setBuilder) BuildPackages(_ context.Context, spec isolation.BuildSpec, out isolation.LineSink) (isolation.RunResult, error) {
	raw, _ := os.ReadFile(filepath.Join(spec.InDir, "request.json"))
	var req struct {
		Requirements string `json:"requirements"`
		FromLock     bool   `json:"from_lock"`
	}
	_ = json.Unmarshal(raw, &req)
	b.mu.Lock()
	b.requests = append(b.requests, req.Requirements)
	b.mu.Unlock()
	out.Line("== installing")
	res := map[string]any{"ok": true, "lock": torchLock, "modules": []string{"torch"}}
	if strings.Contains(req.Requirements, "badpkg") {
		res = map[string]any{"ok": false, "error": "no wheel for badpkg"}
	} else {
		_ = os.MkdirAll(filepath.Join(spec.OutDir, "site-packages", "torch"), 0o755)
		_ = os.WriteFile(filepath.Join(spec.OutDir, "site-packages", "torch", "__init__.py"), []byte("\n"), 0o644)
	}
	j, _ := json.Marshal(res)
	_ = os.WriteFile(filepath.Join(spec.OutDir, "result.json"), j, 0o644)
	return isolation.RunResult{}, nil
}

func (*setBuilder) RemoveBuildOrphans(context.Context, string) error { return nil }

type pkgEnv struct {
	*env
	svc   *packages.Service
	mount chan string // each run's PackagesDir
}

// newPkgEnv: package sets on, dependency_check reject (the notebook
// imports torch, which the image lacks).
func newPkgEnv(t *testing.T) pkgEnv {
	t.Helper()
	var svc *packages.Service
	e := newEnvWith(t, func(d *Deps, db *store.DB, data string) {
		_ = os.MkdirAll(filepath.Join(data, "tmp"), 0o700)
		svc = packages.New(packages.Config{Enabled: true, IndexURL: "https://pypi.org/simple", MaxSize: 1 << 20, BuildTimeout: time.Minute,
			Retention: 24 * time.Hour, CacheSize: 10 << 20, BuilderImage: "b", Network: "bridge", DataDir: data, WorkerID: "w1",
			Platform: "linux/amd64"}, db, blob.NewMemory(), &setBuilder{}, nil)
		d.Packages = svc
	}, func(c *Config) { c.DependencyCheck = "reject" })
	mount := make(chan string, 16)
	e.be.mu.Lock()
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		mount <- spec.PackagesDir
		return succeed(time.Millisecond)(ctx, spec, out)
	}
	e.be.mu.Unlock()
	return pkgEnv{env: e, svc: svc, mount: mount}
}

func (e pkgEnv) put(t *testing.T, sp, name, content string) {
	t.Helper()
	done := false
	if _, err := e.sp.Upload(context.Background(), "alice", sp, "", func() (*spaces.UploadPart, error) {
		if done {
			return nil, io.EOF
		}
		done = true
		return &spaces.UploadPart{Name: name, Body: strings.NewReader(content)}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (e pkgEnv) builds(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.svc.Run(ctx)
}

func TestRunWaitsForItsPackageSet(t *testing.T) {
	e := newPkgEnv(t)
	ctx := context.Background()
	sp := e.space(t, "alice")
	// Without requirements.txt, reject refuses the torch import.
	var de *DependencyError
	if _, _, err := e.m.Submit(ctx, "alice", sp, SubmitRequest{Notebook: "nb.ipynb"}); !errors.As(err, &de) {
		t.Fatalf("torch without a set: %v", err)
	}
	e.put(t, sp, "requirements.txt", "torch\n")
	if v, _ := e.m.SpacePackages(ctx, "alice", sp); v.Status != "not_built" {
		t.Fatalf("before: %+v", v)
	}
	// The set isn't built: what it provides isn't known, so no refusal.
	v := e.submit(t, "alice", sp, "")
	if v.Status != StatusQueued || v.WaitingFor != "packages" {
		t.Fatalf("waiting: %+v", v)
	}
	time.Sleep(100 * time.Millisecond)
	if len(e.mount) != 0 {
		t.Fatal("started before its set was ready")
	}
	e.builds(t)
	if done := e.wait(t, "alice", sp, v.ID, true); done.Status != StatusSucceeded {
		t.Fatalf("with the set: %+v", done)
	}
	if dir := <-e.mount; dir == "" {
		t.Fatal("the set wasn't mounted")
	}
	// Ready: its modules count as provided, so reject accepts torch.
	v2, _, err := e.m.Submit(ctx, "alice", sp, SubmitRequest{Notebook: "nb.ipynb"})
	if err != nil || len(v2.DependencyWarnings) != 0 || v2.WaitingFor != "" {
		t.Fatalf("second run: %+v %v", v2, err)
	}
	e.wait(t, "alice", sp, v2.ID, true)
	pv, err := e.m.SpacePackages(ctx, "alice", sp)
	if err != nil || pv.Status != "ready" || len(pv.Installed) != 1 || pv.Installed[0].Name != "torch" || pv.Installed[0].Version != "2.5.0" {
		t.Fatalf("packages: %+v %v", pv, err)
	}
	if bv, err := e.m.BuildSpacePackages(ctx, "alice", sp); err != nil || bv.Status != "ready" {
		t.Errorf("build again: %+v %v", bv, err)
	}
	if _, lock, _ := e.db.SpacePackages(ctx, sp); lock != torchLock {
		t.Errorf("the space's lock: %q", lock)
	}

	// Publish pins the lock; an invocation installs exactly it.
	vv, err := e.m.Publish(ctx, "alice", sp, "f", PublishRequest{Notebook: "nb.ipynb"})
	if err != nil {
		t.Fatal(err)
	}
	fv, _ := e.db.GetFunctionVersion(ctx, sp, "f", vv.Version)
	if fv.PackageLock != torchLock {
		t.Fatalf("published lock: %q", fv.PackageLock)
	}
	inv, _, err := e.m.Invoke(ctx, "alice", sp, "f", InvokeRequest{Parameters: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if done := e.wait(t, "alice", sp, inv.ID, true); done.Status != StatusSucceeded {
		t.Fatalf("invocation: %+v", done)
	}
}

func TestPackageSetFailures(t *testing.T) {
	e := newPkgEnv(t)
	ctx := context.Background()
	sp := e.space(t, "alice")
	e.put(t, sp, "requirements.txt", "--index-url http://169.254.169.254/\n")
	var invalid *spaces.InvalidError
	if _, _, err := e.m.Submit(ctx, "alice", sp, SubmitRequest{Notebook: "nb.ipynb"}); !errors.As(err, &invalid) {
		t.Fatalf("index option: %v", err)
	}
	if v, _ := e.m.SpacePackages(ctx, "alice", sp); v.Status != "failed" || v.Error == "" {
		t.Errorf("shown: %+v", v)
	}
	e.put(t, sp, "requirements.txt", "badpkg\ntorch\n")
	v := e.submit(t, "alice", sp, "")
	e.builds(t)
	done := e.wait(t, "alice", sp, v.ID, true)
	if done.Status != StatusFailed || done.ErrorKind != KindPackages || !strings.Contains(done.Message, "badpkg") {
		t.Fatalf("waiting run: %+v", done)
	}
	var pf *PackagesFailedError
	if _, _, err := e.m.Submit(ctx, "alice", sp, SubmitRequest{Notebook: "nb.ipynb"}); !errors.As(err, &pf) || !strings.Contains(pf.Error(), "badpkg") {
		t.Fatalf("next run: %v", err)
	}
	if _, err := e.m.Publish(ctx, "alice", sp, "f", PublishRequest{Notebook: "nb.ipynb"}); !errors.As(err, &pf) {
		t.Errorf("publish: %v", err)
	}
	if bv, err := e.m.BuildSpacePackages(ctx, "alice", sp); err != nil || bv.Status != "failed" {
		t.Errorf("build a failed set: %+v %v", bv, err)
	}
	other := e.space(t, "alice")
	if _, err := e.m.BuildSpacePackages(ctx, "alice", other); !errors.As(err, &invalid) {
		t.Errorf("no requirements.txt: %v", err)
	}
	if v, _ := e.m.SpacePackages(ctx, "alice", other); v.Status != "none" {
		t.Errorf("none: %+v", v)
	}
	if _, err := e.m.SpacePackages(ctx, "bob", sp); !errors.Is(err, spaces.ErrNotFound) {
		t.Errorf("bob: %v", err)
	}
}

func TestPackagesOff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sp := e.space(t, "alice")
	if v, err := e.m.SpacePackages(ctx, "alice", sp); err != nil || v.Enabled || v.Status != "disabled" {
		t.Errorf("off: %+v %v", v, err)
	}
	var invalid *spaces.InvalidError
	if _, err := e.m.BuildSpacePackages(ctx, "alice", sp); !errors.As(err, &invalid) {
		t.Errorf("build while off: %v", err)
	}
}

func TestWithoutProvided(t *testing.T) {
	ws := []deps.Warning{{Module: "torch", Source: "notebook"}, {Module: "sklearn", Source: "notebook"}, {Module: "scikit-learn", Source: "requirements.txt"}}
	if kept, unsure := withoutProvided(ws, store.PackageSet{}); len(kept) != 3 || unsure {
		t.Errorf("no set: %v %v", kept, unsure)
	}
	ready := store.PackageSet{Key: "k", Status: store.PackagesReady, Modules: []string{"sklearn"}}
	if kept, unsure := withoutProvided(ws, ready); len(kept) != 1 || kept[0].Module != "torch" || unsure {
		t.Errorf("ready: %v %v", kept, unsure)
	}
	pending := store.PackageSet{Key: "k", Status: store.PackagesPending, Requirements: "torch\nscikit-learn\n"}
	if kept, unsure := withoutProvided(ws, pending); len(kept) != 1 || kept[0].Module != "sklearn" || !unsure {
		t.Errorf("pending: %v %v", kept, unsure)
	}
}

// Administrators list and stop any owner's runs, and drain the workers.
func TestAdminRunsAndDrain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.be.mu.Lock()
	e.be.behave = succeed(time.Hour)
	e.be.mu.Unlock()
	sp := e.space(t, "alice")
	v := e.submit(t, "alice", sp, "")
	e.wait(t, "alice", sp, v.ID, false)
	list, _, err := e.m.AdminList(ctx, AdminFilter{Owner: "alice", Status: StatusRunning}, "", 10)
	if err != nil || len(list) != 1 || list[0].Owner != "alice" || list[0].Result != nil {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, f := range []AdminFilter{{Status: "nope"}, {SpaceID: "nope"}} {
		if _, _, err := e.m.AdminList(ctx, f, "", 10); err == nil {
			t.Errorf("%+v accepted", f)
		}
	}
	if _, _, err := e.m.AdminList(ctx, AdminFilter{}, "garbage", 10); err == nil {
		t.Error("bad cursor accepted")
	}
	if _, err := e.m.AdminCancel(ctx, v.ID, "root", strings.Repeat("x", MaxCancelReason+1)); err == nil {
		t.Error("long reason accepted")
	}
	if _, err := e.m.AdminCancel(ctx, "00000000-0000-4000-8000-000000000000", "root", ""); !errors.Is(err, spaces.ErrNotFound) {
		t.Errorf("unknown run: %v", err)
	}
	c, err := e.m.AdminCancel(ctx, v.ID, "root", "runaway")
	if err != nil || c.Owner != "alice" {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	if done := e.wait(t, "alice", sp, v.ID, true); done.Status != StatusCancelled || done.CancelledBy != "root" {
		t.Fatalf("cancelled: %+v", done)
	}

	if _, err := e.m.SetDrain(ctx, true, strings.Repeat("x", MaxDrainReason+1), "root"); err == nil {
		t.Error("long drain reason accepted")
	}
	d, err := e.m.SetDrain(ctx, true, "upgrade", "root")
	if err != nil || !d.Draining || d.By != "root" {
		t.Fatalf("drain: %+v %v", d, err)
	}
	e.be.mu.Lock()
	e.be.behave = succeed(time.Millisecond)
	e.be.mu.Unlock()
	queued := e.submit(t, "alice", sp, "")
	time.Sleep(100 * time.Millisecond)
	if st, _ := e.m.DrainState(ctx); st.Queued != 1 {
		t.Fatalf("while drained: %+v", st)
	}
	if _, err := e.m.SetDrain(ctx, false, "", "root"); err != nil {
		t.Fatal(err)
	}
	if done := e.wait(t, "alice", sp, queued.ID, true); done.Status != StatusSucceeded {
		t.Fatalf("after resuming: %+v", done)
	}
}

// A set that failed before its run was recorded still fails the run.
func TestRunRecordedAfterItsSetFailed(t *testing.T) {
	e := newPkgEnv(t)
	ctx := context.Background()
	sp := e.space(t, "alice")
	now := time.Now()
	if _, err := e.db.RequestPackageSet(ctx, store.PackageRequest{Key: "k-failed", Requirements: "badpkg\n"}, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	_, _ = e.db.ClaimPackageBuild(ctx, "w1", time.Minute, now)
	_, _ = e.db.FinishPackageBuild(ctx, "k-failed", "w1", store.BuildOutcome{Reason: "no wheel for badpkg"}, now)
	nbFiles, err := e.sp.CreateRun(ctx, "00000000-0000-4000-8000-0000000000aa", []byte(nb), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.db.SubmitRunWithEvent(ctx, store.NewRun{ID: "00000000-0000-4000-8000-0000000000aa", SpaceID: sp, Owner: "alice",
		Notebook: "nb.ipynb", ParametersJSON: "{}", CreatedAt: now, PackageKey: "k-failed", Files: nbFiles}, nil, nil); err != nil {
		t.Fatal(err)
	}
	e.m.failIfPackagesFailed(ctx, "00000000-0000-4000-8000-0000000000aa", "k-failed")
	if r, _ := e.db.RunByID(ctx, "00000000-0000-4000-8000-0000000000aa"); r.Status != StatusFailed || r.ErrorKind != KindPackages {
		t.Fatalf("run: %+v", r)
	}
}
