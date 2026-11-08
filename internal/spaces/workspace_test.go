// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

func readSpaceFile(t *testing.T, e env, space, p string) string {
	t.Helper()
	f, _, err := e.svc.OpenFile(context.Background(), "alice", space, p)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(f)
	return string(b)
}

func listPaths(t *testing.T, e env, space string) []string {
	t.Helper()
	fs, err := e.svc.ListFiles(context.Background(), "alice", space)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		p := f.Path
		if f.Type == TypeFolder {
			p += "/"
		}
		out = append(out, p)
	}
	return out
}

// A run works on a copy of the space; its changes come back when it ends,
// each checked against the version it started from.
func TestCheckoutCheckin(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 10<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("notes.txt", "start", "report.csv", "a,b", "gone.txt", "bye")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "data", parts("in.csv", "1,2")); err != nil {
		t.Fatal(err)
	}
	before, _ := e.svc.Usage(ctx, sp.ID)

	runID := NewID()
	dir, err := e.svc.Checkout(ctx, sp.ID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "data", "in.csv")); string(b) != "1,2" {
		t.Fatalf("checked-out data/in.csv = %q", b)
	}
	// The working copy doesn't count twice toward the quota.
	e.svc.InvalidateUsage(sp.ID)
	if u, _ := e.svc.Usage(ctx, sp.ID); u.BytesUsed != before.BytesUsed {
		t.Fatalf("usage with a working copy: %d, before %d", u.BytesUsed, before.BytesUsed)
	}

	// What the run does.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("SECRET"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("start\nrun was here\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "report.csv"), []byte("run,version"), 0o600)
	_ = os.Remove(filepath.Join(dir, "gone.txt"))
	_ = os.MkdirAll(filepath.Join(dir, "out", "empty"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "out", "result.txt"), []byte("42"), 0o600)
	_ = os.Symlink(outside, filepath.Join(dir, "leak.txt"))
	_ = os.Symlink("/etc", filepath.Join(dir, "etc"))

	// Meanwhile someone edits report.csv.
	cur, _ := e.svc.Stat(ctx, "alice", sp.ID, "report.csv")
	if _, err := e.svc.WriteFile(ctx, "alice", sp.ID, WriteRequest{Path: "report.csv", Body: strings.NewReader("edited,meanwhile"), IfMatch: cur.Version}); err != nil {
		t.Fatal(err)
	}

	conflicts, skipped, err := e.svc.Checkin(ctx, sp.ID, runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Discard(sp.ID, runID)
	saved := store.ConflictPath("report.csv", runID[:8])
	if len(conflicts) != 1 || conflicts[0].Path != "report.csv" || conflicts[0].SavedAs != saved {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	slices.Sort(skipped)
	if !slices.Equal(skipped, []string{"etc", "leak.txt"}) {
		t.Fatalf("skipped = %v (links must not be stored)", skipped)
	}
	want := []string{"data/", "data/in.csv", "notes.txt", "out/", "out/empty/", "out/result.txt", "report.conflict-" + runID[:8] + ".csv", "report.csv"}
	if got := listPaths(t, e, sp.ID); !slices.Equal(got, want) {
		t.Fatalf("space after the run:\n got %v\nwant %v", got, want)
	}
	if got := readSpaceFile(t, e, sp.ID, "report.csv"); got != "edited,meanwhile" {
		t.Fatalf("the edit must win: %q", got)
	}
	if got := readSpaceFile(t, e, sp.ID, saved); got != "run,version" {
		t.Fatalf("the run's version: %q", got)
	}
	if got := readSpaceFile(t, e, sp.ID, "notes.txt"); got != "start\nrun was here\n" {
		t.Fatalf("notes.txt = %q", got)
	}
	if b, _ := os.ReadFile(outside); string(b) != "SECRET" {
		t.Fatal("the file a link pointed to was touched")
	}
	if _, err := os.Stat(e.svc.WorkPath(sp.ID, runID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Discard left the working copy")
	}
}

// A run's changes over the quota are not applied at all.
func TestCheckinOverQuota(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 4<<10)
	sp, _ := e.svc.Create(ctx, "alice", "")
	if _, err := e.svc.Upload(ctx, "alice", sp.ID, "", parts("a.txt", "a")); err != nil {
		t.Fatal(err)
	}
	runID := NewID()
	dir, err := e.svc.Checkout(ctx, sp.ID, runID)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 8<<10), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o600)
	if _, _, err := e.svc.Checkin(ctx, sp.ID, runID, 0); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("over quota: %v", err)
	}
	e.svc.Discard(sp.ID, runID)
	if got := listPaths(t, e, sp.ID); !slices.Equal(got, []string{"a.txt"}) {
		t.Fatalf("nothing must be applied: %v", got)
	}
	if got := readSpaceFile(t, e, sp.ID, "a.txt"); got != "a" {
		t.Fatalf("a.txt = %q", got)
	}
}

// A run that changes nothing writes nothing back.
func TestCheckinNoChanges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<20, 1<<20)
	sp, _ := e.svc.Create(ctx, "alice", "")
	_, _ = e.svc.Upload(ctx, "alice", sp.ID, "", parts("a.txt", "a"))
	before, _ := e.svc.Stat(ctx, "alice", sp.ID, "a.txt")
	runID := NewID()
	if _, err := e.svc.Checkout(ctx, sp.ID, runID); err != nil {
		t.Fatal(err)
	}
	if c, s, err := e.svc.Checkin(ctx, sp.ID, runID, 0); err != nil || len(c) != 0 || len(s) != 0 {
		t.Fatalf("checkin: %v %v %v", c, s, err)
	}
	if after, _ := e.svc.Stat(ctx, "alice", sp.ID, "a.txt"); !after.Modified.Equal(before.Modified) {
		t.Fatal("an unchanged file was rewritten")
	}
}
