// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

func runLog(t *testing.T, e *env, spaceID, runID string) string {
	t.Helper()
	f, _, err := e.m.OpenOutput(context.Background(), "alice", spaceID, runID, spaces.RunLog)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(f)
	return string(b)
}

// A run works on a copy of the space; its changes come back when it ends.
// An edit made meanwhile wins, the run's version is kept beside it, and the
// run's log says so; links the run made are not stored.
func TestRunWritesBack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sid := e.space(t, "alice")
	release := make(chan struct{})
	started := make(chan struct{})
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		_ = os.WriteFile(filepath.Join(spec.SpaceDir, "result.txt"), []byte("42"), 0o600)
		_ = os.WriteFile(filepath.Join(spec.SpaceDir, "nb.ipynb"), []byte(nb+" "), 0o600)
		_ = os.Symlink("/etc/passwd", filepath.Join(spec.SpaceDir, "passwd"))
		close(started)
		<-release
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
	v := e.submit(t, "alice", sid, "")
	<-started
	// While the run works, the notebook is saved again.
	cur, err := e.sp.Stat(ctx, "alice", sid, "nb.ipynb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.sp.WriteFile(ctx, "alice", sid, spaces.WriteRequest{Path: "nb.ipynb", Body: strings.NewReader(nb + "\n\n"), IfMatch: cur.Version}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if v = e.wait(t, "alice", sid, v.ID, true); v.Status != StatusSucceeded {
		t.Fatalf("run %s", v.Status)
	}
	files, _ := e.sp.ListFiles(ctx, "alice", sid)
	var names []string
	for _, f := range files {
		names = append(names, f.Path)
	}
	conflict := "nb.conflict-" + v.ID[:8] + ".ipynb"
	if strings.Join(names, ",") != conflict+",nb.ipynb,result.txt" {
		t.Fatalf("space after the run: %v", names)
	}
	log := runLog(t, e, sid, v.ID)
	for _, want := range []string{"nb.ipynb was changed while the run worked", conflict, "not stored in the space", "passwd"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if _, err := os.Stat(e.sp.WorkPath(sid, v.ID)); !os.IsNotExist(err) {
		t.Fatal("the working copy was left behind")
	}
}

// Changes over the quota are not applied, and the log says why.
func TestRunWriteBackOverQuota(t *testing.T) {
	e := newEnv(t)
	sid := e.space(t, "alice")
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		_ = os.WriteFile(filepath.Join(spec.SpaceDir, "big.bin"), make([]byte, 11<<20), 0o600)
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
	v := e.submit(t, "alice", sid, "")
	e.wait(t, "alice", sid, v.ID, true)
	if _, err := e.sp.Stat(context.Background(), "alice", sid, "big.bin"); err == nil {
		t.Fatal("changes over the quota were applied")
	}
	if log := runLog(t, e, sid, v.ID); !strings.Contains(log, "would exceed the space's quota") {
		t.Fatalf("log doesn't explain:\n%s", log)
	}
}

// A read-only run's changes are discarded.
func TestReadOnlyRunDiscards(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sid := e.space(t, "alice")
	e.be.behave = func(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
		if !spec.SpaceReadOnly {
			t.Error("a read-only run got a writable space")
		}
		_ = os.WriteFile(filepath.Join(spec.SpaceDir, "x.txt"), []byte("x"), 0o600) // as if the mount allowed it
		writeStatus(spec, "succeeded", nil)
		return isolation.RunResult{}, nil
	}
	v, _, err := e.m.Submit(ctx, "alice", sid, SubmitRequest{Notebook: "nb.ipynb", ReadOnlySpace: true})
	if err != nil {
		t.Fatal(err)
	}
	e.wait(t, "alice", sid, v.ID, true)
	if _, err := e.sp.Stat(ctx, "alice", sid, "x.txt"); err == nil {
		t.Fatal("a read-only run's file was stored")
	}
	if _, err := os.Stat(e.sp.WorkPath(sid, v.ID)); !os.IsNotExist(err) {
		t.Fatal("the working copy was left behind")
	}
}
