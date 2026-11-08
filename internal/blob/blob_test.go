// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/blob/blobtest"
)

func TestMemoryPassesSuite(t *testing.T) {
	blobtest.Run(t, func(*testing.T) blob.Store { return blob.NewMemory() })
}

func TestValidKey(t *testing.T) {
	for _, ok := range []string{"a", "sha256/ab/cd/abcd", "runs/0d0f/log.txt", "a-b_c.d"} {
		if !blob.ValidKey(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/..", "/a", "a/", "A", "a b", "a\\b", "a//b"} {
		if blob.ValidKey(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if !blob.ValidPrefix("") || !blob.ValidPrefix("a/") || blob.ValidPrefix("../") || blob.ValidPrefix("/") {
		t.Fatal("prefix rules")
	}
}

func TestContentKey(t *testing.T) {
	sum := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if got := blob.ContentKey(sum); got != "sha256/ab/cd/"+sum || !blob.ValidKey(got) {
		t.Fatalf("ContentKey = %q", got)
	}
}

func TestExactReader(t *testing.T) {
	read := func(src string, size int64) (string, bool, error) {
		er := blob.NewExactReader(strings.NewReader(src), size)
		b, err := io.ReadAll(er)
		return string(b), er.Mismatch(), err
	}
	if got, mm, err := read("hello", 5); got != "hello" || mm || err != nil {
		t.Fatalf("exact: %q %v %v", got, mm, err)
	}
	if _, mm, err := read("hel", 5); !mm || !errors.Is(err, blob.ErrSizeMismatch) {
		t.Fatalf("short: %v %v", mm, err)
	}
	if _, mm, err := read("hello!", 5); !mm || !errors.Is(err, blob.ErrSizeMismatch) {
		t.Fatalf("long: %v %v", mm, err)
	}
	if got, mm, err := read("", 0); got != "" || mm || err != nil {
		t.Fatalf("empty: %q %v %v", got, mm, err)
	}
	// io.ReadFull keeps an error that comes with the last bytes it wanted
	// only if those bytes are withheld: a too-long source must make it fail.
	buf := make([]byte, 3)
	if _, err := io.ReadFull(blob.NewExactReader(strings.NewReader("hello"), 3), buf); err == nil {
		t.Fatal("io.ReadFull over a too-long source succeeded: an adapter would store a cut-off object")
	}
}
