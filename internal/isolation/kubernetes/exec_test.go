// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestStreamOrAbandonReturnsTheStreamsResult(t *testing.T) {
	want := errors.New("tar: exit 2")
	abandoned := false
	err := streamOrAbandon(context.Background(), func(context.Context) error { return want }, func() { abandoned = true })
	if !errors.Is(err, want) || abandoned {
		t.Fatalf("err %v, abandoned %v; want the stream's error, not abandoned", err, abandoned)
	}
}

func TestStreamOrAbandonWaitsForAStreamThatEndsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	abandoned := false
	err := streamOrAbandon(ctx, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, func() { abandoned = true })
	if !errors.Is(err, context.Canceled) || abandoned {
		t.Fatalf("err %v, abandoned %v; want canceled, not abandoned", err, abandoned)
	}
}

// The case behind a run that never ended on Kubernetes: the stream ignores
// its context (client-go blocked closing a stalled connection).
func TestStreamOrAbandonLeavesAStuckStream(t *testing.T) {
	defer func(d time.Duration) { abandonAfter = d }(abandonAfter)
	abandonAfter = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	abandoned := false
	start := time.Now()
	err := streamOrAbandon(ctx, func(context.Context) error { <-release; return nil }, func() { abandoned = true })
	if !errors.Is(err, context.DeadlineExceeded) || !abandoned {
		t.Fatalf("err %v, abandoned %v; want the deadline, abandoned", err, abandoned)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("returned after %v", d)
	}
}

func TestGateDropsWritesOnceClosed(t *testing.T) {
	var buf bytes.Buffer
	g := &gate{w: &buf}
	if n, err := g.Write([]byte("kept\n")); n != 5 || err != nil {
		t.Fatal(n, err)
	}
	g.close()
	if n, err := g.Write([]byte("late\n")); n != 5 || err != nil {
		t.Fatal(n, err)
	}
	if buf.String() != "kept\n" || g.String() != "kept\n" {
		t.Fatalf("buffer %q, String %q", buf.String(), g.String())
	}
}
