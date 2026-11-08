// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

// Cleanup removes only contents nothing refers to and that are old enough;
// contents shared by another file stay.
func TestCollectGarbage(t *testing.T) {
	ctx := context.Background()
	mem := blob.NewMemory()
	svc, err := New(Config{DataDir: t.TempDir(), MaxFile: 1 << 20, MaxSpace: 10 << 20}, storetest.Open(t), mem,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := svc.Create(ctx, "alice", "")
	old := time.Now().Add(-3 * time.Hour)
	mem.Now = func() time.Time { return old }
	if _, err := svc.Upload(ctx, "alice", sp.ID, "", parts("a.txt", "shared", "b.txt", "shared", "c.txt", "only c")); err != nil {
		t.Fatal(err)
	}
	mem.Now = time.Now
	// An object an upload has just stored but not yet recorded: recent, kept.
	sum, _, err := blob.PutContent(ctx, mem, bytes.NewReader([]byte("in flight")), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteFile(ctx, "alice", sp.ID, "a.txt"); err != nil { // b.txt still uses the same contents
		t.Fatal(err)
	}
	if err := svc.DeleteFile(ctx, "alice", sp.ID, "c.txt"); err != nil {
		t.Fatal(err)
	}
	n, err := svc.CollectGarbage(ctx, CleanupGrace)
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v; want 1 (c.txt's contents only)", n, err)
	}
	if got := readSpaceFile(t, env{svc: svc}, sp.ID, "b.txt"); got != "shared" {
		t.Fatalf("shared contents removed: %q", got)
	}
	if _, err := mem.Head(ctx, blob.ContentKey(sum)); err != nil {
		t.Fatal("recent unreferenced contents (an upload in flight) were removed")
	}
	if n, _ := svc.CollectGarbage(ctx, CleanupGrace); n != 0 {
		t.Fatalf("second pass removed %d", n)
	}
}
