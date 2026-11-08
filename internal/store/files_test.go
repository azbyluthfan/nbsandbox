// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func sha(c byte) string { return strings.Repeat(string(c), 64) }

func paths(t *testing.T, db *DB, space string) []string {
	t.Helper()
	fs, err := db.ListFiles(context.Background(), space)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		p := f.Path
		if f.Kind == "folder" {
			p += "/"
		}
		out = append(out, p)
	}
	return out
}

func TestFileIndex(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	now := time.Now()
	none := ""

	// An upload: parents become folders; all or nothing.
	if err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "data/a.csv", SHA256: sha('a'), Size: 10}, {Path: "nb.ipynb", SHA256: sha('b'), Size: 5, If: &none}}, now, 0); err != nil {
		t.Fatal(err)
	}
	if got := paths(t, db, "s1"); !slices.Equal(got, []string{"data/", "data/a.csv", "nb.ipynb"}) {
		t.Fatalf("after upload: %v", got)
	}
	// "Must not exist" refuses an existing file; the whole batch is undone.
	err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "new.txt", SHA256: sha('c'), Size: 1}, {Path: "nb.ipynb", SHA256: sha('c'), Size: 1, If: &none}}, now, 0)
	var vm *VersionMismatch
	if !errors.As(err, &vm) || vm.Current != sha('b') {
		t.Fatalf("create over an existing file: %v", err)
	}
	if _, err := db.GetFile(ctx, "s1", "new.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a refused batch must store nothing")
	}
	// A save with the right version works; with a stale one it's refused.
	old := sha('b')
	if err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "nb.ipynb", SHA256: sha('d'), Size: 6, If: &old}}, now, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "nb.ipynb", SHA256: sha('e'), Size: 6, If: &old}}, now, 0); !errors.As(err, &vm) || vm.Current != sha('d') {
		t.Fatalf("stale save: %v", err)
	}
	// Paths through a file, and files over folders, are refused.
	if err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "nb.ipynb/x", SHA256: sha('f'), Size: 1}}, now, 0); !errors.Is(err, ErrNotDir) {
		t.Fatalf("file under a file: %v", err)
	}
	if err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "data", SHA256: sha('f'), Size: 1}}, now, 0); !errors.Is(err, ErrIsDir) {
		t.Fatalf("file over a folder: %v", err)
	}
	// Quota: counted after the change.
	if err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "big.bin", SHA256: sha('g'), Size: 100}}, now, 50); !errors.Is(err, ErrQuota) {
		t.Fatalf("over quota: %v", err)
	}
	if n, _ := db.SpaceFileBytes(ctx, "s1"); n != 16 {
		t.Fatalf("space bytes = %d, want 16", n)
	}

	// Folders: create, move with contents, delete.
	if err := db.CreateFolder(ctx, "s1", "reports/2026", now); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateFolder(ctx, "s1", "reports", now); !errors.Is(err, ErrExists) {
		t.Fatalf("existing folder: %v", err)
	}
	if _, err := db.MovePath(ctx, "s1", "data", "archive/data", nil, now); err != nil {
		t.Fatal(err)
	}
	if got := paths(t, db, "s1"); !slices.Equal(got, []string{"archive/", "archive/data/", "archive/data/a.csv", "nb.ipynb", "reports/", "reports/2026/"}) {
		t.Fatalf("after moving a folder: %v", got)
	}
	if _, err := db.MovePath(ctx, "s1", "archive", "archive/inside", nil, now); err == nil {
		t.Fatal("a folder moved into itself")
	}
	if _, err := db.MovePath(ctx, "s1", "nb.ipynb", "reports", nil, now); !errors.Is(err, ErrExists) {
		t.Fatalf("move onto an existing path: %v", err)
	}
	stale := sha('b')
	if _, err := db.MovePath(ctx, "s1", "nb.ipynb", "x.ipynb", &stale, now); !errors.As(err, &vm) {
		t.Fatalf("move with a stale version: %v", err)
	}
	if err := db.DeletePath(ctx, "s1", "archive", false); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("delete a non-empty folder without recursive: %v", err)
	}
	if err := db.DeletePath(ctx, "s1", "archive", true); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePath(ctx, "s1", "archive", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if got := paths(t, db, "s1"); !slices.Equal(got, []string{"nb.ipynb", "reports/", "reports/2026/"}) {
		t.Fatalf("after delete: %v", got)
	}

	// A folder name with LIKE wildcards matches only itself.
	if err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "a_b/x", SHA256: sha('h'), Size: 1}, {Path: "acb/y", SHA256: sha('i'), Size: 1}}, now, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePath(ctx, "s1", "a_b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetFile(ctx, "s1", "acb/y"); err != nil {
		t.Fatal("deleting a_b/ removed acb/: LIKE wildcards not escaped")
	}

	// Referenced contents, for cleanup.
	var refs []string
	if err := db.ReferencedContent(ctx, func(s string) error { refs = append(refs, s); return nil }); err != nil {
		t.Fatal(err)
	}
	slices.Sort(refs)
	if !slices.Equal(refs, []string{sha('d'), sha('i')}) {
		t.Fatalf("referenced = %v", refs)
	}
	// Another space's operations never see these files.
	seedSpace(t, db, "s2")
	if got := paths(t, db, "s2"); len(got) != 0 {
		t.Fatalf("space s2 sees %v", got)
	}
	if err := db.PutFiles(ctx, "missing", []FileWrite{{Path: "x", SHA256: sha('j'), Size: 1}}, now, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write to a missing space: %v", err)
	}
}

func TestApplyRunChanges(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	now := time.Now()
	if err := db.PutFiles(ctx, "s1", []FileWrite{
		{Path: "notes.txt", SHA256: sha('a'), Size: 1}, {Path: "edited.csv", SHA256: sha('b'), Size: 1},
		{Path: "old/gone.txt", SHA256: sha('c'), Size: 1}, {Path: "kept.txt", SHA256: sha('d'), Size: 1},
	}, now, 0); err != nil {
		t.Fatal(err)
	}
	// Someone edits edited.csv and kept.txt while the run works.
	if err := db.PutFiles(ctx, "s1", []FileWrite{{Path: "edited.csv", SHA256: sha('x'), Size: 2}, {Path: "kept.txt", SHA256: sha('y'), Size: 2}}, now, 0); err != nil {
		t.Fatal(err)
	}
	a, b, c, d, none := sha('a'), sha('b'), sha('c'), sha('d'), ""
	conflicts, err := db.ApplyRunChanges(ctx, "s1",
		[]FileWrite{
			{Path: "notes.txt", SHA256: sha('e'), Size: 3, If: &a},      // clean change
			{Path: "edited.csv", SHA256: sha('f'), Size: 3, If: &b},     // edited meanwhile: kept beside
			{Path: "out/new.txt", SHA256: sha('g'), Size: 3, If: &none}, // new
		},
		[]FileDelete{{Path: "old/gone.txt", If: c}, {Path: "kept.txt", If: d}, {Path: "old", Folder: true}},
		[]string{"empty/dir"}, "run12345", now, 0, Lease{})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(t, db, "s1"); !slices.Equal(got, []string{"edited.conflict-run12345.csv", "edited.csv", "empty/", "empty/dir/", "kept.txt", "notes.txt", "out/", "out/new.txt"}) {
		t.Fatalf("after applying: %v", got)
	}
	if f, _ := db.GetFile(ctx, "s1", "edited.csv"); f.SHA256 != sha('x') {
		t.Fatal("the newer edit must win")
	}
	if len(conflicts) != 2 || conflicts[0].SavedAs != "edited.conflict-run12345.csv" || conflicts[1].Path != "kept.txt" || conflicts[1].SavedAs != "" {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	// Over quota: nothing is applied.
	n := sha('n')
	if _, err := db.ApplyRunChanges(ctx, "s1", []FileWrite{{Path: "big.bin", SHA256: n, Size: 1000}}, nil, nil, "r2", now, 100, Lease{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("over quota: %v", err)
	}
	if _, err := db.GetFile(ctx, "s1", "big.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a refused write-back must store nothing")
	}
	if got := ConflictPath("a/b/report", "t"); got != "a/b/report.conflict-t" {
		t.Fatalf("ConflictPath without extension = %q", got)
	}
}

func TestRunAndVersionFiles(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	in := []ContentRef{{Path: "input.ipynb", SHA256: sha('a'), Size: 10}, {Path: "params.json", SHA256: sha('b'), Size: 2}}
	if _, _, _, err := db.SubmitRunWithEvent(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "nb.ipynb",
		CreatedAt: time.Now(), Files: in}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Outputs, and a result rewritten: upserted.
	if err := db.PutRunFiles(ctx, "r1", 0, []ContentRef{{Path: "result.json", SHA256: sha('c'), Size: 5}, {Path: "artifacts/x.csv", SHA256: sha('d'), Size: 7}}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutRunFiles(ctx, "r1", 0, []ContentRef{{Path: "result.json", SHA256: sha('e'), Size: 6}}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutRunFiles(ctx, "r1", 0, nil); err != nil {
		t.Fatal(err)
	}
	files, err := db.RunFiles(ctx, "r1")
	if err != nil || len(files) != 4 {
		t.Fatalf("run files %+v %v", files, err)
	}
	if f, err := db.RunFile(ctx, "r1", "result.json"); err != nil || f.SHA256 != sha('e') || f.Size != 6 {
		t.Fatalf("result %+v %v", f, err)
	}
	if _, err := db.RunFile(ctx, "r1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run file: %v", err)
	}
	if n, err := db.SpaceRunBytes(ctx, "s1"); err != nil || n != 10+2+6+7 {
		t.Fatalf("run bytes %d %v", n, err)
	}
	if b, c, err := db.SpaceFileStats(ctx, "s1"); err != nil || b != 0 || c != 0 {
		t.Fatalf("file stats %d %d %v", b, c, err)
	}

	// A function version's files.
	vf := []ContentRef{{Path: "nb.ipynb", SHA256: sha('f'), Size: 3}, {Path: "m/x.pkl", SHA256: sha('a'), Size: 10}}
	if err := db.AddFunctionVersion(ctx, FunctionVersion{SpaceID: "s1", Function: "fn", Version: 1, Notebook: "nb.ipynb", FilesJSON: "[]",
		NotebookSHA256: sha('f'), BundleBytes: 13, CreatedBy: "alice", CreatedAt: time.Now(), Files: vf}); err != nil {
		t.Fatal(err)
	}
	if got, err := db.FunctionVersionFiles(ctx, "s1", "fn", 1); err != nil || len(got) != 2 {
		t.Fatalf("version files %+v %v", got, err)
	}
	if got, _ := db.FunctionVersionFiles(ctx, "s1", "fn", 2); len(got) != 0 {
		t.Fatalf("missing version files %+v", got)
	}
	if n, err := db.SpacePublishedBytes(ctx, "s1"); err != nil || n != 13 {
		t.Fatalf("published bytes %d %v", n, err)
	}

	var refs []string
	if err := db.ReferencedContent(ctx, func(s string) error { refs = append(refs, s); return nil }); err != nil {
		t.Fatal(err)
	}
	slices.Sort(refs)
	if !slices.Equal(refs, []string{sha('a'), sha('b'), sha('d'), sha('e'), sha('f')}) {
		t.Fatalf("referenced = %v", refs)
	}
}
