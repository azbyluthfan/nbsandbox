// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package completions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
)

func TestPublishAndOpen(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMemory()
	p := NewPublisher(ctx, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if p.Ref(nil) != nil {
		t.Fatal("a ref without an index")
	}
	index := []byte("gzipped index")
	var ref *Ref
	for deadline := time.Now().Add(5 * time.Second); ref == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		ref = p.Ref(index) // nil until stored
	}
	if ref == nil || len(ref.SHA256) != 64 || ref.Bytes != int64(len(index)) {
		t.Fatalf("ref %+v", ref)
	}
	rc, err := Open(ctx, store, *ref)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "gzipped index" {
		t.Fatalf("read %q", b)
	}
	// A new image's index replaces it; the old one is no longer named.
	other := []byte("another index")
	if r := p.Ref(other); r != nil && r.SHA256 == ref.SHA256 {
		t.Fatal("the old ref for a new index")
	}
	for _, bad := range []Ref{{SHA256: "../../etc/passwd"}, {SHA256: "ab"}, {SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}} {
		if _, err := Open(ctx, store, bad); !errors.Is(err, ErrNone) {
			t.Errorf("%q: %v", bad.SHA256, err)
		}
	}
}
