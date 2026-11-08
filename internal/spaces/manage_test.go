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

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

func TestWriteFileVersions(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<10, 4<<10)
	sp, _ := e.svc.Create(ctx, "alice", "")
	w := func(req WriteRequest) (File, error) { return e.svc.WriteFile(ctx, "alice", sp.ID, req) }

	f1, err := w(WriteRequest{Path: "src/model.py", Body: strings.NewReader("x = 1\n"), IfNoneMatch: true})
	if err != nil || f1.Version == "" || f1.Type != TypeFile {
		t.Fatalf("create: %+v %v", f1, err)
	}
	var ve *VersionError
	if _, err := w(WriteRequest{Path: "src/model.py", Body: strings.NewReader("y"), IfNoneMatch: true}); !errors.As(err, &ve) || ve.Current != f1.Version {
		t.Fatalf("create over an existing file: %v", err)
	}
	if _, err := w(WriteRequest{Path: "src/model.py", Body: strings.NewReader("y")}); !errors.As(err, &ve) || !ve.Required {
		t.Fatalf("replace without If-Match: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // versions include the modification time
	f2, err := w(WriteRequest{Path: "src/model.py", Body: strings.NewReader("x = 2\n"), IfMatch: f1.Version})
	if err != nil || f2.Version == f1.Version {
		t.Fatalf("replace: %+v %v", f2, err)
	}
	// A second editor still holding the first version is refused, and told
	// the current one.
	if _, err := w(WriteRequest{Path: "src/model.py", Body: strings.NewReader("z"), IfMatch: f1.Version}); !errors.As(err, &ve) || ve.Current != f2.Version || ve.Required {
		t.Fatalf("stale If-Match: %v", err)
	}
	if f, _, err := e.svc.OpenFile(ctx, "alice", sp.ID, "src/model.py"); err != nil {
		t.Fatal(err)
	} else {
		b, _ := io.ReadAll(f)
		_ = f.Close()
		if string(b) != "x = 2\n" {
			t.Fatalf("content %q", b)
		}
	}
	if _, err := w(WriteRequest{Path: "gone.txt", Body: strings.NewReader("z"), IfMatch: "abc"}); !errors.As(err, &ve) || ve.Current != "" {
		t.Fatalf("If-Match on a missing file: %v", err)
	}
	// Limits and validation as for uploads; nothing is left behind.
	var le *LimitError
	if _, err := w(WriteRequest{Path: "big.bin", Body: strings.NewReader(strings.Repeat("x", 2<<10))}); !errors.As(err, &le) {
		t.Fatalf("over the file limit: %v", err)
	}
	var ie *InvalidError
	if _, err := w(WriteRequest{Path: "nb.ipynb", Body: strings.NewReader("not json")}); !errors.As(err, &ie) {
		t.Fatalf("invalid notebook: %v", err)
	}
	if _, err := w(WriteRequest{Path: "src", Body: strings.NewReader("x")}); err == nil {
		t.Fatal("wrote over a folder")
	}
	list, _ := e.svc.ListFiles(ctx, "alice", sp.ID)
	if len(list) != 2 {
		t.Fatalf("leftovers after failed writes: %+v", list)
	}
	if _, err := e.svc.WriteFile(ctx, "bob", sp.ID, WriteRequest{Path: "x", Body: strings.NewReader("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatal("bob wrote into alice's space")
	}
}

func TestFoldersMoveDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	_, _ = e.svc.Upload(ctx, "alice", sp.ID, "data", parts("a.csv", "1", "b.csv", "2"))

	if f, err := e.svc.CreateFolder(ctx, "alice", sp.ID, "empty/inner"); err != nil || f.Type != TypeFolder {
		t.Fatalf("create folder: %+v %v", f, err)
	}
	if _, err := e.svc.CreateFolder(ctx, "alice", sp.ID, "data"); !errors.Is(err, ErrExists) {
		t.Fatalf("existing folder: %v", err)
	}
	if _, err := e.svc.CreateFolder(ctx, "alice", sp.ID, "data/a.csv/x"); err == nil {
		t.Fatal("folder under a file")
	}

	// Rename a file, move a folder, refuse clashes and moving into itself.
	a, _ := e.svc.Stat(ctx, "alice", sp.ID, "data/a.csv")
	var ve *VersionError
	if _, err := e.svc.Move(ctx, "alice", sp.ID, "data/a.csv", "data/renamed.csv", "stale"); !errors.As(err, &ve) {
		t.Fatalf("move with a stale version: %v", err)
	}
	if _, err := e.svc.Move(ctx, "alice", sp.ID, "data/a.csv", "data/renamed.csv", a.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Move(ctx, "alice", sp.ID, "data/b.csv", "data/renamed.csv", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("move onto an existing file: %v", err)
	}
	if _, err := e.svc.Move(ctx, "alice", sp.ID, "data", "data/sub", ""); err == nil {
		t.Fatal("moved a folder into itself")
	}
	if f, err := e.svc.Move(ctx, "alice", sp.ID, "data", "archive/2026", ""); err != nil || f.Type != TypeFolder {
		t.Fatalf("move folder: %+v %v", f, err)
	}
	if _, err := e.svc.Move(ctx, "alice", sp.ID, "missing", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("move missing: %v", err)
	}

	// Delete: files, empty folders; non-empty only when recursive.
	if err := e.svc.DeletePath(ctx, "alice", sp.ID, "archive", false); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("non-empty folder: %v", err)
	}
	if err := e.svc.DeletePath(ctx, "alice", sp.ID, "empty/inner", false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePath(ctx, "alice", sp.ID, "archive", true); err != nil {
		t.Fatal(err)
	}
	var paths []string
	list, _ := e.svc.ListFiles(ctx, "alice", sp.ID)
	for _, f := range list {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, " ") != "empty" {
		t.Fatalf("after deletes: %v", paths)
	}
	if u, _ := e.svc.Usage(ctx, sp.ID); u.BytesUsed != 0 || u.FileCount != 0 {
		t.Fatalf("usage after deletes: %+v", u)
	}
}

func TestPromoteArtifact(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	submitRun(t, e, sp.ID, runID, "{}")
	art := filepath.Join(e.svc.RunPath(sp.ID, runID), RunArtifacts, "model.pkl")
	_ = os.WriteFile(art, []byte("MODEL"), 0o600)
	f, err := e.svc.PromoteArtifact(ctx, "alice", sp.ID, runID, "model.pkl", "models/v1.pkl")
	if err != nil || f.Path != "models/v1.pkl" {
		t.Fatalf("promote: %+v %v", f, err)
	}
	r, _, err := e.svc.OpenFile(ctx, "alice", sp.ID, "models/v1.pkl")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "MODEL" {
		t.Fatalf("promoted %q", b)
	}
	if _, err := e.svc.PromoteArtifact(ctx, "alice", sp.ID, runID, "model.pkl", "models/v1.pkl"); !errors.Is(err, ErrExists) {
		t.Fatalf("promote onto an existing file: %v", err)
	}
	if _, err := e.svc.PromoteArtifact(ctx, "alice", sp.ID, runID, "missing.pkl", "x.pkl"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing artifact: %v", err)
	}
	if _, err := e.svc.PromoteArtifact(ctx, "alice", sp.ID, runID, "../input.ipynb", "x.ipynb"); err == nil {
		t.Fatal("promoted a file outside the artifacts folder")
	}
	// After the run, from object storage.
	if err := e.svc.StoreRunOutputs(ctx, sp.ID, runID, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := e.svc.PromoteArtifact(ctx, "alice", sp.ID, runID, "model.pkl", "models/v2.pkl"); err != nil || f.Size != 5 {
		t.Fatalf("promote after the run: %+v %v", f, err)
	}
}

func TestBundles(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 64<<10)
	sp, _ := e.svc.Create(ctx, "alice", "")
	_, _ = e.svc.Upload(ctx, "alice", sp.ID, "", parts("nb.ipynb", validNB))
	_, _ = e.svc.Upload(ctx, "alice", sp.ID, "models", parts("m.pkl", "MODEL", "readme.txt", "x"))
	_, _ = e.svc.Upload(ctx, "alice", sp.ID, "models/sub", parts("w.bin", "WW"))

	refs, n, err := e.svc.BundleFiles(ctx, sp.ID, "nb.ipynb", []string{"models", "models/m.pkl"})
	if err != nil || n != int64(len(validNB)+5+1+2) || len(refs) != 4 {
		t.Fatalf("bundle: %v %d %v", refs, n, err)
	}
	if err := e.db.AddFunctionVersion(ctx, store.FunctionVersion{SpaceID: sp.ID, Function: "fn", Version: 1, Notebook: "nb.ipynb",
		FilesJSON: "[]", NotebookSHA256: refs[0].SHA256, BundleBytes: n, CreatedBy: "alice", CreatedAt: time.Now(), Files: refs}); err != nil {
		t.Fatal(err)
	}
	// Editing the space afterwards doesn't change the version.
	_, _ = e.svc.Upload(ctx, "alice", sp.ID, "models", parts("m.pkl", "CHANGED"))
	if b, err := e.svc.ReadBundleNotebook(ctx, sp.ID, "fn", 1, "nb.ipynb"); err != nil || string(b) != validNB {
		t.Fatalf("bundle notebook: %v", err)
	}
	if b, err := e.svc.ContentOf(ctx, refs[0].SHA256, 1<<20); err != nil || string(b) != validNB {
		t.Fatalf("content of the notebook: %v", err)
	}
	if _, err := e.svc.ReadBundleNotebook(ctx, sp.ID, "fn", 2, "nb.ipynb"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing version read")
	}
	run := NewID()
	dir, err := e.svc.CheckoutBundle(ctx, sp.ID, "fn", 1, run)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"nb.ipynb": validNB, "models/m.pkl": "MODEL", "models/readme.txt": "x", "models/sub/w.bin": "WW"} {
		if b, err := os.ReadFile(filepath.Join(dir, p)); err != nil || string(b) != want {
			t.Errorf("%s in the checked-out bundle: %q %v", p, b, err)
		}
	}
	e.svc.Discard(sp.ID, run)
	var ie *InvalidError
	for name, c := range map[string]struct {
		nb    string
		files []string
	}{
		"missing file": {"nb.ipynb", []string{"nope"}},
		"folder as nb": {"models", nil},
		"bad path":     {"../nb.ipynb", nil},
	} {
		if _, _, err := e.svc.BundleFiles(ctx, sp.ID, c.nb, c.files); err == nil || (name != "bad path" && !errors.As(err, &ie)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Published versions count toward the quota.
	if u, _ := e.svc.Usage(ctx, sp.ID); u.BytesUsed < 2*n {
		t.Fatalf("published version not counted: %+v", u)
	}
	_, _ = e.svc.Upload(ctx, "alice", sp.ID, "", parts("big.bin", strings.Repeat("x", 40<<10)))
	var le *LimitError
	if _, _, err := e.svc.BundleFiles(ctx, sp.ID, "nb.ipynb", []string{"big.bin"}); !errors.As(err, &le) {
		t.Fatalf("over quota: %v", err)
	}
	if !ValidFunctionName("forecast-v2_eu") || ValidFunctionName("Forecast") || ValidFunctionName("-x") || ValidFunctionName("") {
		t.Fatal("ValidFunctionName")
	}
	if f, err := e.svc.Stat(ctx, "alice", sp.ID, "models"); err != nil || f.Type != TypeFolder {
		t.Fatalf("stat folder: %+v %v", f, err)
	}
	if _, err := e.svc.Stat(ctx, "alice", sp.ID, "passwd-link"); !errors.Is(err, ErrNotFound) {
		t.Fatal("stat followed a link")
	}
}
