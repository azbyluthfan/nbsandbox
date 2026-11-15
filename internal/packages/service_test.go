// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package packages

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

// fakeBuilder does what build_packages.py would: it reads the request and
// writes a result and a site-packages folder.
type fakeBuilder struct {
	mu       sync.Mutex
	requests []buildRequest
	result   buildResult
	files    map[string]string // site-packages contents
	orphans  []string
}

func (f *fakeBuilder) BuildPackages(_ context.Context, spec isolation.BuildSpec, out isolation.LineSink) (isolation.RunResult, error) {
	var req buildRequest
	b, err := os.ReadFile(filepath.Join(spec.InDir, "request.json"))
	if err != nil {
		return isolation.RunResult{}, err
	}
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	out.Line("== installing from requirements.txt")
	for name, content := range f.files {
		p := filepath.Join(spec.OutDir, "site-packages", name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(content), 0o644)
	}
	rb, _ := json.Marshal(f.result)
	_ = os.WriteFile(filepath.Join(spec.OutDir, "result.json"), rb, 0o644)
	return isolation.RunResult{}, nil
}

func (f *fakeBuilder) RemoveBuildOrphans(_ context.Context, worker string) error {
	f.orphans = append(f.orphans, worker)
	return nil
}

const testLock = "humanize==4.17.0 --hash=sha256:02d0899527760efe18011944f882df4970943b4cdff7b5a0ba7fed43a576ef98\n"

type env struct {
	db      *store.DB
	blobs   *blob.Memory
	builder *fakeBuilder
	svc     *Service
	space   string
}

func newEnv(t *testing.T, mod func(*Config)) env {
	t.Helper()
	db := storetest.Open(t)
	space := "00000000-0000-4000-8000-000000000001"
	now := time.Now()
	if err := db.CreateSpace(context.Background(), store.Space{ID: space, Owner: "alice", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	_ = os.Mkdir(filepath.Join(data, "tmp"), 0o700)
	cfg := Config{Enabled: true, IndexURL: "https://pypi.org/simple", MaxSize: 1 << 20, BuildTimeout: time.Minute, Retention: 24 * time.Hour,
		CacheSize: 10 << 20, BuilderImage: "builder:test", Network: "bridge", DataDir: data, WorkerID: "w1", Platform: "linux/amd64"}
	if mod != nil {
		mod(&cfg)
	}
	b := &fakeBuilder{result: buildResult{OK: true, Lock: testLock, Used: "requirements.txt", Modules: []string{"humanize"}},
		files: map[string]string{"humanize/__init__.py": "VERSION = '4.17.0'\n", "humanize-4.17.0.dist-info/METADATA": "Name: humanize\n"}}
	mem := blob.NewMemory()
	return env{db: db, blobs: mem, builder: b, svc: New(cfg, db, mem, b, nil), space: space}
}

func TestForSpaceRefusesBeforeBuilding(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Allowed = []string{"humanize", "requests"} })
	ctx := context.Background()
	var invalid *InvalidError
	if _, err := e.svc.ForSpace(ctx, e.space, []byte("-i http://169.254.169.254/\n"), "img"); !errors.As(err, &invalid) {
		t.Errorf("an index option: %v", err)
	}
	if _, err := e.svc.ForSpace(ctx, e.space, []byte("humanize\nevil-pkg\n"), "img"); !errors.As(err, &invalid) || !strings.Contains(err.Error(), "evil-pkg") {
		t.Errorf("not allowed: %v", err)
	}
	if k, err := e.svc.ForSpace(ctx, e.space, []byte("# nothing\n"), "img"); k != "" || err != nil {
		t.Errorf("no requirements: %q %v", k, err)
	}
	k, err := e.svc.ForSpace(ctx, e.space, []byte("humanize\n"), "img")
	if err != nil || len(k) != 64 {
		t.Fatalf("asked: %q %v", k, err)
	}
	if ps, _ := e.svc.Status(ctx, k); ps.Status != store.PackagesPending {
		t.Errorf("pending: %+v", ps)
	}
	// Another runner image: another set.
	if k2, _ := e.svc.ForSpace(ctx, e.space, []byte("humanize\n"), "img2"); k2 == k {
		t.Error("the runner image is part of the key")
	}
}

func TestBuildStoreAndUse(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	var finished []string
	e.svc.OnFinished = func(key string, ps store.PackageSet) { finished = append(finished, key+":"+ps.Status) }
	key, err := e.svc.ForSpace(ctx, e.space, []byte("humanize\n"), "img")
	if err != nil {
		t.Fatal(err)
	}
	if !e.svc.buildOne(ctx) {
		t.Fatal("nothing to build")
	}
	ps, _ := e.svc.Status(ctx, key)
	if ps.Status != store.PackagesReady || ps.Lock != testLock || ps.SizeBytes == 0 || !strings.Contains(ps.Log, "installing") ||
		len(ps.Modules) != 1 || ps.Modules[0] != "humanize" {
		t.Fatalf("ready: %+v", ps)
	}
	if len(finished) != 1 || finished[0] != key+":ready" {
		t.Errorf("finished: %v", finished)
	}
	if _, err := e.blobs.Head(ctx, blobKey(key)); err != nil {
		t.Errorf("stored: %v", err)
	}
	if e.svc.buildOne(ctx) {
		t.Error("built twice")
	}
	// The worker that built it has it unpacked already.
	dir, err := e.svc.Ensure(ctx, key)
	if b, _ := os.ReadFile(filepath.Join(dir, "humanize", "__init__.py")); err != nil || string(b) != "VERSION = '4.17.0'\n" {
		t.Fatalf("on the builder: %q %v", b, err)
	}
	// Another worker downloads and unpacks it (read-only files).
	other := New(e.svc.cfg, e.db, e.blobs, nil, nil)
	other.cfg.DataDir = t.TempDir()
	dir, err = other.Ensure(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "humanize", "__init__.py"))
	if err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("unpacked: %v %v", info, err)
	}

	// The space keeps its lock: the next build of the same requirements
	// (another runner image) tries it first.
	e.svc.RecordSpaceLock(ctx, e.space, ps)
	if _, err := e.svc.ForSpace(ctx, e.space, []byte("humanize\n"), "img2"); err != nil {
		t.Fatal(err)
	}
	e.svc.buildOne(ctx)
	if last := e.builder.requests[len(e.builder.requests)-1]; last.PreferLock != testLock || last.IndexURL != "https://pypi.org/simple" {
		t.Errorf("the second build's request: %+v", last)
	}
	// A published version's lock is installed exactly.
	lk, err := e.svc.ForLock(ctx, testLock, "img")
	if err != nil || lk == key {
		t.Fatalf("lock set: %q %v", lk, err)
	}
	e.svc.buildOne(ctx)
	if last := e.builder.requests[len(e.builder.requests)-1]; !last.FromLock || last.Requirements != testLock {
		t.Errorf("lock build: %+v", last)
	}
}

func TestBuildFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		mod    func(*env)
		reason string
	}{
		"pip failed":          {func(e *env) { e.builder.result = buildResult{OK: false, Error: "no wheel for x"} }, "no wheel for x"},
		"too large":           {func(e *env) { e.builder.files["big.bin"] = strings.Repeat("x", 2<<20) }, "more than"},
		"a dependency banned": {func(e *env) { e.svc.cfg.Allowed = []string{"other"} }, "doesn't allow: humanize"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			ctx := context.Background()
			key, err := e.svc.ForSpace(ctx, e.space, []byte("other\n"), "img")
			if err != nil {
				t.Fatal(err)
			}
			tc.mod(&e)
			e.svc.buildOne(ctx)
			ps, _ := e.svc.Status(ctx, key)
			if ps.Status != store.PackagesFailed || !strings.Contains(ps.Error, tc.reason) {
				t.Fatalf("%+v", ps)
			}
			// Asked again soon: refused at once, with the reason.
			var failed *FailedError
			if _, err := e.svc.ForSpace(ctx, e.space, []byte("other\n"), "img"); !errors.As(err, &failed) || failed.Reason != ps.Error {
				t.Errorf("asked again: %v", err)
			}
		})
	}
}

func TestReadTarRefusesEscapes(t *testing.T) {
	mk := func(hs ...tar.Header) []byte {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for _, h := range hs {
			h.Mode = 0o644
			_ = tw.WriteHeader(&h)
			if h.Typeflag == tar.TypeReg {
				_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			}
		}
		_ = tw.Close()
		return b.Bytes()
	}
	for name, data := range map[string][]byte{
		"parent":   mk(tar.Header{Name: "../evil.py", Typeflag: tar.TypeReg, Size: 1}),
		"absolute": mk(tar.Header{Name: "/etc/evil", Typeflag: tar.TypeReg, Size: 1}),
		"too big":  mk(tar.Header{Name: "a", Typeflag: tar.TypeReg, Size: 100}, tar.Header{Name: "b", Typeflag: tar.TypeReg, Size: 100}),
	} {
		if err := readTar(bytes.NewReader(data), t.TempDir(), 150); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	if err := readTar(bytes.NewReader(mk(tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		tar.Header{Name: "ok/a.py", Typeflag: tar.TypeReg, Size: 3})), dir, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Error("a symlink was unpacked")
	}
}

func TestRunRemovesOrphansAndSweeps(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	key, _ := e.svc.ForSpace(ctx, e.space, []byte("humanize\n"), "img")
	e.svc.buildOne(ctx)
	e.svc.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	runCtx, cancel := context.WithCancel(ctx)
	cancel()
	e.svc.Run(runCtx)
	if len(e.builder.orphans) != 1 || e.builder.orphans[0] != "w1" {
		t.Errorf("orphans: %v", e.builder.orphans)
	}
	e.svc.sweep(ctx)
	if _, err := e.svc.Status(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unused set kept: %v", err)
	}
	if _, err := e.blobs.Head(ctx, blobKey(key)); err == nil {
		t.Error("its object kept")
	}
}
