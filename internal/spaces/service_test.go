// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

const validNB = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": []}`

type env struct {
	svc   *Service
	db    *store.DB
	data  string
	blobs *blob.Memory
}

func newEnv(t *testing.T, maxFile, maxSpace int64) env {
	t.Helper()
	data := t.TempDir()
	db := storetest.Open(t)
	blobs := blob.NewMemory()
	svc, err := New(Config{DataDir: data, MaxFile: maxFile, MaxSpace: maxSpace, MaxFilesPerUpload: 5}, db, blobs,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return env{svc: svc, db: db, data: data, blobs: blobs}
}

func parts(files ...string) NextPart { // name, body, name, body, ...
	i := 0
	return func() (*UploadPart, error) {
		if i >= len(files) {
			return nil, io.EOF
		}
		p := &UploadPart{Name: files[i], Body: strings.NewReader(files[i+1])}
		i += 2
		return p, nil
	}
}

func (e env) entries(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(e.svc.Dir(id), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(e.svc.Dir(id), p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func TestSpaceLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, err := e.svc.Create(ctx, "alice", "  forecasts  ")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(sp.ID) || sp.Name != "forecasts" {
		t.Fatalf("created %+v", sp)
	}
	if info, err := os.Stat(e.svc.Dir(sp.ID)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("space dir: %v %v", info, err)
	}
	for _, sub := range []string{RunsDir, WorkDir} {
		if _, err := os.Stat(filepath.Join(e.svc.Dir(sp.ID), sub)); err != nil {
			t.Fatalf("%s/ not created", sub)
		}
	}

	// Ownership: bob sees nothing (404, never 403).
	if _, err := e.svc.Get(ctx, "bob", sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob Get: %v", err)
	}
	if _, err := e.svc.Rename(ctx, "bob", sp.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob Rename: %v", err)
	}
	if err := e.svc.Delete(ctx, "bob", sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob Delete: %v", err)
	}
	if list, _, _ := e.svc.List(ctx, "bob", "", 10); len(list) != 0 {
		t.Fatalf("bob List: %v", list)
	}

	got, err := e.svc.Rename(ctx, "alice", sp.ID, "sales")
	if err != nil || got.Name != "sales" {
		t.Fatalf("rename: %+v %v", got, err)
	}
	if _, err := e.svc.Rename(ctx, "alice", sp.ID, strings.Repeat("x", MaxNameBytes+1)); err == nil {
		t.Fatal("long name must be rejected")
	}
	if _, err := e.svc.Create(ctx, "alice", "bad\x07name"); err == nil {
		t.Fatal("control character in name must be rejected")
	}

	// Delete is refused while a run is active, then removes everything.
	_ = e.db.InsertRun(ctx, store.NewRun{ID: "r1", SpaceID: sp.ID, Owner: "alice", Notebook: "a.ipynb", CreatedAt: time.Now()})
	if err := e.svc.Delete(ctx, "alice", sp.ID); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("delete with active run: %v", err)
	}
	_, _ = e.db.FinishRun(ctx, "r1", store.Outcome{Status: "succeeded", FinishedAt: time.Now()})
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("a.csv", "1,2")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, "alice", sp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.svc.Dir(sp.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("space dir still exists: %v", err)
	}
	if _, err := e.svc.Get(ctx, "alice", sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted space Get: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(e.data, "spaces")); len(left) != 0 {
		t.Fatalf("leftovers in spaces/: %v", left)
	}
}

func TestUploadListDownloadDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")

	files, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("forecast.ipynb", validNB, "notes.txt", "hi"))
	if err != nil || len(files) != 2 {
		t.Fatalf("upload: %v %v", files, err)
	}
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "data/2026", parts("sales.csv", "a,b\n1,2\n")); err != nil {
		t.Fatal(err)
	}
	// Replacing a file is allowed.
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("notes.txt", "hello again")); err != nil {
		t.Fatal(err)
	}

	list, err := e.svc.ListFiles(ctx, "alice", sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range list {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, " ") != "data data/2026 data/2026/sales.csv forecast.ipynb notes.txt" {
		t.Fatalf("list = %v", paths)
	}

	f, info, err := e.svc.OpenFile(ctx, "alice", sp.ID, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "hello again" || info.Size != 11 {
		t.Fatalf("download = %q", b)
	}

	u, _ := e.svc.Usage(ctx, sp.ID)
	if u.FileCount != 3 || u.BytesUsed != int64(len(validNB)+11+8) {
		t.Fatalf("usage = %+v", u)
	}

	if err := e.svc.DeleteFile(ctx, "alice", sp.ID, "notes.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteFile(ctx, "alice", sp.ID, "notes.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := e.svc.DeleteFile(ctx, "alice", sp.ID, "data"); err == nil {
		t.Fatal("deleting a folder must be refused")
	}
	if _, _, err := e.svc.OpenFile(ctx, "alice", sp.ID, "data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("downloading a folder: %v", err)
	}
	if u, _ := e.svc.Usage(ctx, sp.ID); u.FileCount != 2 {
		t.Fatalf("usage after delete = %+v", u)
	}
	// bob can't touch any of it.
	if _, err := e.svc.ListFiles(ctx, "bob", sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("bob ListFiles")
	}
	if _, _, err := e.svc.OpenFile(ctx, "bob", sp.ID, "forecast.ipynb"); !errors.Is(err, ErrNotFound) {
		t.Fatal("bob OpenFile")
	}
	if err := e.svc.DeleteFile(ctx, "bob", sp.ID, "forecast.ipynb"); !errors.Is(err, ErrNotFound) {
		t.Fatal("bob DeleteFile")
	}
	if _, err := e.svc.Upload(ctx, "bob", sp.ID, "", parts("x.txt", "x")); !errors.Is(err, ErrNotFound) {
		t.Fatal("bob Upload")
	}
}

// Over limits → 413 and nothing stored.
func TestUploadLimitsStoreNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 100, 250)
	sp, _ := e.svc.Create(ctx, "alice", "")
	ok := strings.Repeat("x", 100)

	cases := map[string]NextPart{
		"file over limit":         parts("a.txt", "small", "big.txt", strings.Repeat("x", 101)),
		"quota over limit":        parts("a.txt", ok, "b.txt", ok, "c.txt", ok),
		"too many files":          parts("1", "", "2", "", "3", "", "4", "", "5", "", "6", ""),
		"bad notebook mid-upload": parts("a.txt", ok, "bad.ipynb", `{"nbformat": 3, "cells": []}`),
		"duplicate name":          parts("a.txt", "1", "a.txt", "2"),
		"folder in part name":     parts("../x.txt", "1"),
	}
	for name, p := range cases {
		_, err := e.svc.Upload(ctx, "alice", sp.ID, "", p)
		if err == nil {
			t.Errorf("%s: want error", name)
		}
		if left := e.entries(t, sp.ID); len(left) != 0 {
			t.Errorf("%s: files left behind: %v", name, left)
		}
	}
	var le *LimitError
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("big.txt", strings.Repeat("x", 101))); !errors.As(err, &le) {
		t.Fatalf("file limit error type: %T %v", err, err)
	}
	// Exactly at the limits is fine.
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("a.txt", ok, "b.txt", ok)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("c.txt", strings.Repeat("x", 51))); !errors.As(err, &le) {
		t.Fatalf("quota: %v", err)
	}
}

// A non-notebook submitted as a notebook → 400.
func TestNotebookValidation(t *testing.T) {
	bad := map[string]string{
		"not json":    "hello",
		"no nbformat": `{"cells": []}`,
		"nbformat 3":  `{"nbformat": 3, "cells": []}`,
		"no cells":    `{"nbformat": 4}`,
	}
	for name, body := range bad {
		if err := ValidateNotebook(strings.NewReader(body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if err := ValidateNotebook(strings.NewReader(validNB)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	var ie *InvalidError
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("x.IPYNB", "not json")); !errors.As(err, &ie) {
		t.Fatalf("upper-case extension must be validated too: %v", err)
	}
}

// Traversal reaches nothing outside the space: paths are validated first,
// and then they are only keys in the space's file index.
func TestTraversal(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	other, _ := e.svc.Create(ctx, "bob", "")
	if _, err := e.svc.Upload(ctx, "bob", other.ID, "", parts("canary.txt", "SECRET")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../" + other.ID + "/canary.txt", "../../" + other.ID + "/canary.txt", "/etc/passwd", "a/../../x", "canary.txt"} {
		if f, _, err := e.svc.OpenFile(ctx, "alice", sp.ID, p); err == nil {
			b, _ := io.ReadAll(f)
			_ = f.Close()
			t.Fatalf("OpenFile(%q) read %q", p, b)
		}
	}
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "../"+other.ID, parts("x.txt", "x")); err == nil {
		t.Fatal("upload to ../ must fail")
	}
	if list, _ := e.svc.ListFiles(ctx, "bob", other.ID); len(list) != 1 {
		t.Fatalf("bob's space changed: %v", list)
	}
	// Run outputs on this host are never reachable through the files API.
	_ = os.WriteFile(filepath.Join(e.svc.Dir(sp.ID), RunsDir, "r.txt"), []byte("run output"), 0o600)
	if _, _, err := e.svc.OpenFile(ctx, "alice", sp.ID, "runs/r.txt"); err == nil {
		t.Fatal("run outputs readable through files API")
	}
	if list, _ := e.svc.ListFiles(ctx, "alice", sp.ID); len(list) != 0 {
		t.Fatalf("run outputs listed: %v", list)
	}
	e.svc.InvalidateUsage(sp.ID)
	if u, _ := e.svc.Usage(ctx, sp.ID); u.BytesUsed != 10 || u.FileCount != 0 {
		t.Fatalf("run outputs count toward bytes but not files: %+v", u)
	}
}

func TestPaging(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	for i := 0; i < 3; i++ {
		_, _ = e.svc.Create(ctx, "alice", "")
	}
	first, next, err := e.svc.List(ctx, "alice", "", 2)
	if err != nil || len(first) != 2 || next == "" {
		t.Fatalf("page 1: %d %q %v", len(first), next, err)
	}
	second, next2, _ := e.svc.List(ctx, "alice", next, 2)
	if len(second) != 1 || next2 != "" {
		t.Fatalf("page 2: %d %q", len(second), next2)
	}
	var ie *InvalidError
	if _, _, err := e.svc.List(ctx, "alice", "garbage!", 2); !errors.As(err, &ie) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if !ValidID(id) || seen[id] {
			t.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "x", "0D0F8A8E-1111-4111-8111-111111111111", "0d0f8a8e11114111811111111111111"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}
