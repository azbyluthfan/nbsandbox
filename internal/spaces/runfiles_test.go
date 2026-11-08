// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

const runID = "33333333-3333-4333-8333-333333333333"

// submitRun records a run the way the run manager does: the snapshot and
// parameters stored, the row inserted with them, the local folder prepared.
func submitRun(t *testing.T, e env, spaceID, id string, params string) {
	t.Helper()
	ctx := context.Background()
	refs, err := e.svc.CreateRun(ctx, id, []byte(validNB), []byte(params))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.db.SubmitRunWithEvent(ctx, store.NewRun{ID: id, SpaceID: spaceID, Owner: "alice", Notebook: "nb.ipynb",
		CreatedAt: time.Now(), ReadOnlySpace: true, Files: refs}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PrepareRun(ctx, spaceID, id); err != nil {
		t.Fatal(err)
	}
}

func TestRunFiles(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	if err := e.svc.Owned(ctx, "alice", sp.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Owned(ctx, "bob", sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("bob owns alice's space")
	}
	if _, err := e.svc.CreateRun(ctx, "../x", []byte(validNB), nil); err == nil {
		t.Fatal("a run id that isn't a UUID was accepted")
	}
	submitRun(t, e, sp.ID, runID, `{"a":1}`)
	if err := e.svc.PrepareRun(ctx, sp.ID, runID); err == nil {
		t.Fatal("preparing a run's folder twice must fail (a run executes once)")
	}
	w, err := e.svc.OpenRunLog(sp.ID, runID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("line\n"))
	_ = w.Close()
	if _, err := e.svc.OpenRunLog(sp.ID, runID); err == nil {
		t.Fatal("opening the log twice must fail (a run executes once)")
	}

	// While the run works: its local folder.
	dir := e.svc.RunPath(sp.ID, runID)
	_ = os.Symlink(e.svc.Dir(sp.ID), filepath.Join(dir, "artifacts", "space"))
	_ = os.MkdirAll(filepath.Join(dir, "artifacts", "charts"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "artifacts", "summary.csv"), []byte("a,b"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "artifacts", "charts", "c.png"), []byte("png"), 0o600)
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("SECRET"), 0o600)
	// A notebook could plant symlinks in its own run directory.
	_ = os.Symlink(outside, filepath.Join(dir, "artifacts", "leak"))
	_ = os.Symlink(outside, filepath.Join(dir, RunHTML))
	if err := e.svc.WriteRunFile(context.Background(), sp.ID, runID, RunResult, []byte(`{"status":"succeeded"}`)); err != nil {
		t.Fatal(err)
	}

	check := func(phase string) {
		t.Helper()
		b, err := e.svc.ReadRunFile(context.Background(), sp.ID, runID, RunParams, 1024)
		if err != nil || string(b) != `{"a":1}` {
			t.Fatalf("%s: params %q %v", phase, b, err)
		}
		if b, _ := e.svc.ReadRunFile(context.Background(), sp.ID, runID, RunInput, 1<<20); string(b) != validNB {
			t.Fatalf("%s: snapshot %q", phase, b)
		}
		if n, ok := e.svc.RunFileSize(context.Background(), sp.ID, runID, RunLog); !ok || n != 5 {
			t.Fatalf("%s: log size %d %v", phase, n, ok)
		}
		if b, _ := e.svc.ReadRunFile(context.Background(), sp.ID, runID, RunResult, 1024); string(b) != `{"status":"succeeded"}` {
			t.Fatalf("%s: result %q", phase, b)
		}
		if _, err := e.svc.ReadRunFile(context.Background(), sp.ID, runID, RunResult, 3); err == nil {
			t.Fatalf("%s: size limit not enforced", phase)
		}
		arts, err := e.svc.ListRunArtifacts(context.Background(), sp.ID, runID)
		if err != nil || len(arts) != 2 || arts[0].Path != "charts/c.png" || arts[1].Path != "summary.csv" {
			t.Fatalf("%s: artifacts %+v %v", phase, arts, err)
		}
		f, _, err := e.svc.OpenRunFile(context.Background(), sp.ID, runID, "artifacts/summary.csv")
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		got, _ := io.ReadAll(f)
		_ = f.Close()
		if string(got) != "a,b" {
			t.Fatalf("%s: artifact %q", phase, got)
		}
		for _, rel := range []string{"artifacts/leak", RunHTML, "../../" + sp.ID, "artifacts/space/x", "../" + runID} {
			if f, _, err := e.svc.OpenRunFile(context.Background(), sp.ID, runID, rel); err == nil {
				b, _ := io.ReadAll(f)
				_ = f.Close()
				t.Fatalf("%s: OpenRunFile(%q) read %q", phase, rel, b)
			}
		}
	}
	check("while running")

	// The run ends: its files go to object storage and the folder goes.
	if err := e.svc.StoreRunOutputs(ctx, sp.ID, runID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the run's folder is still there after storing its outputs")
	}
	check("after the run")
	// A result rewritten after the outputs were stored (a lost race).
	if err := e.svc.WriteRunFile(context.Background(), sp.ID, runID, RunResult, []byte(`{"status":"cancelled"}`)); err != nil {
		t.Fatal(err)
	}
	if b, _ := e.svc.ReadRunFile(context.Background(), sp.ID, runID, RunResult, 1024); string(b) != `{"status":"cancelled"}` {
		t.Fatalf("rewritten result %q", b)
	}
	if u, _ := e.svc.Usage(ctx, sp.ID); u.BytesUsed == 0 {
		t.Fatalf("stored run files not counted: %+v", u)
	}
	if arts, _ := e.svc.ListRunArtifacts(context.Background(), sp.ID, "44444444-4444-4444-8444-444444444444"); len(arts) != 0 {
		t.Fatal("artifacts of an unknown run")
	}
	// A run cancelled before any worker prepared it: its result goes
	// straight to object storage.
	queued := NewID()
	refs, _ := e.svc.CreateRun(ctx, queued, []byte(validNB), []byte("{}"))
	if _, _, _, err := e.db.SubmitRunWithEvent(ctx, store.NewRun{ID: queued, SpaceID: sp.ID, Owner: "alice", Notebook: "nb.ipynb",
		CreatedAt: time.Now(), ReadOnlySpace: true, Files: refs}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.WriteRunFile(ctx, sp.ID, queued, RunResult, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if f, info, err := e.svc.OpenRunFile(ctx, sp.ID, queued, RunResult); err != nil || info.Name() != RunResult {
		t.Fatalf("queued run's result: %v %v", info, err)
	} else {
		_ = f.Close()
	}
	if err := e.svc.WriteRunFile(ctx, sp.ID, "nope", RunResult, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad run id: %v", err)
	}
	if err := e.svc.RemoveRun(sp.ID, runID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "SECRET" {
		t.Fatal("symlink target touched")
	}
}

func TestReadNotebookAndSpaceFiles(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	_, _ = e.svc.Upload(ctx, "alice", sp.ID, "", parts("nb.ipynb", validNB, "requirements.txt", "pandas\n", "helpers.py", "x=1"))
	if b, err := e.svc.ReadNotebook(ctx, sp.ID, "nb.ipynb"); err != nil || !strings.Contains(string(b), "nbformat") {
		t.Fatalf("ReadNotebook: %v", err)
	}
	var ie *InvalidError
	for _, p := range []string{"missing.ipynb", "helpers.py"} {
		if _, err := e.svc.ReadNotebook(ctx, sp.ID, p); !errors.As(err, &ie) {
			t.Errorf("ReadNotebook(%q) = %v, want InvalidError", p, err)
		}
	}
	if _, err := e.svc.ReadNotebook(ctx, sp.ID, "../x.ipynb"); err == nil {
		t.Fatal("traversal accepted")
	}
	if b, ok := e.svc.ReadSpaceFile(sp.ID, "requirements.txt", 1024); !ok || string(b) != "pandas\n" {
		t.Fatal("ReadSpaceFile")
	}
	if _, ok := e.svc.ReadSpaceFile(sp.ID, "nope.txt", 1024); ok {
		t.Fatal("missing file read")
	}
	if !e.svc.SpaceEntryExists(sp.ID, "helpers.py") || e.svc.SpaceEntryExists(sp.ID, "nope") || e.svc.SpaceEntryExists(sp.ID, "../x") {
		t.Fatal("SpaceEntryExists")
	}
}

func TestCheckConsistency(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	kept, _ := e.svc.Create(ctx, "alice", "")
	spacesDir := filepath.Join(e.data, "spaces")
	orphan := "55555555-5555-4555-8555-555555555555"
	_ = os.MkdirAll(filepath.Join(spacesDir, orphan, "runs"), 0o700)
	_ = os.MkdirAll(filepath.Join(spacesDir, ".trash-66666666-6666-4666-8666-666666666666", "x"), 0o700)
	_ = os.MkdirAll(filepath.Join(spacesDir, "not-a-space"), 0o700)
	now := time.Now()
	_ = e.db.CreateSpace(ctx, store.Space{ID: "77777777-7777-4777-8777-777777777777", Owner: "alice", CreatedAt: now, UpdatedAt: now})

	if err := e.svc.CheckConsistency(ctx); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(spacesDir)
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	got := strings.Join(names, " ")
	if strings.Contains(got, orphan) || strings.Contains(got, ".trash-") || !strings.Contains(got, kept.ID) || !strings.Contains(got, "not-a-space") {
		t.Fatalf("after check: %s", got)
	}
}

// Contents missing from object storage, or of the wrong size, are errors,
// never wrong data.
func TestRunFilesDamagedStorage(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	submitRun(t, e, sp.ID, runID, `{"a":1}`)
	if err := e.svc.StoreRunOutputs(ctx, sp.ID, runID, 0); err != nil {
		t.Fatal(err)
	}
	ref, err := e.db.RunFile(ctx, runID, RunParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.blobs.Delete(ctx, blob.ContentKey(ref.SHA256)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReadRunFile(ctx, sp.ID, runID, RunParams, 1024); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing contents: %v", err)
	}
	if err := e.blobs.Put(ctx, blob.ContentKey(ref.SHA256), strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if b, err := e.svc.ReadRunFile(ctx, sp.ID, runID, RunParams, 1024); err == nil {
		t.Fatalf("short contents read as %q", b)
	}
	// A second store of the same run (no folder any more) is a no-op.
	if err := e.svc.StoreRunOutputs(ctx, sp.ID, runID, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PrepareRun(ctx, sp.ID, "bad"); err == nil {
		t.Fatal("prepared a run with a bad id")
	}
	if err := e.svc.StoreRunOutputs(ctx, sp.ID, "bad", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("store a bad id: %v", err)
	}
	if _, err := e.svc.CheckoutBundle(ctx, sp.ID, "fn", 1, NewID()); err == nil {
		t.Fatal("checked out a missing version")
	}
	if _, err := e.svc.CheckoutBundle(ctx, sp.ID, "Bad!", 1, NewID()); err == nil {
		t.Fatal("checked out a bad function name")
	}
	if err := e.svc.RemoveRun(sp.ID, "bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove a bad id: %v", err)
	}
}

func TestTempFile(t *testing.T) {
	e := newEnv(t, 1<<20, 10<<20)
	f, info, err := e.svc.TempFile("log.txt", strings.NewReader("a\nb\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(f)
	if string(b) != "a\nb\n" || info.Name() != "log.txt" || info.Size() != 4 {
		t.Fatalf("temp file %q %s %d", b, info.Name(), info.Size())
	}
}
