// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package blobtest is the test suite every blob.Store adapter must pass: the
// in-memory fake in unit tests, and the S3 and Azure adapters against
// RustFS and Azurite in integration tests.
package blobtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
)

// Run runs the suite. newStore returns an empty store; each subtest gets its
// own, or keys under a fresh prefix when the store is shared (keys are
// always made with the prefix the subtest is given).
func Run(t *testing.T, newStore func(t *testing.T) blob.Store) {
	t.Helper()
	ctx := context.Background()
	run := func(name string, f func(t *testing.T, s blob.Store, k func(string) string)) {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			prefix := "blobtest/" + randHex(t, 6) + "/"
			f(t, s, func(key string) string { return prefix + key })
		})
	}

	run("put get head", func(t *testing.T, s blob.Store, k func(string) string) {
		data := []byte("hello, nbsandbox")
		mustPut(ctx, t, s, k("a/one.txt"), data)
		if got := mustGet(ctx, t, s, k("a/one.txt")); !bytes.Equal(got, data) {
			t.Fatalf("get = %q, want %q", got, data)
		}
		info, err := s.Head(ctx, k("a/one.txt"))
		if err != nil || info.Size != int64(len(data)) {
			t.Fatalf("head = %+v, %v", info, err)
		}
		// Cleanup relies on when objects were written (clocks may differ a bit).
		if d := time.Since(info.Modified); d < -2*time.Minute || d > 2*time.Minute {
			t.Fatalf("head: modified %s, now %s", info.Modified, time.Now())
		}
		_ = s.List(ctx, k("a/"), func(i blob.Info) error {
			if d := time.Since(i.Modified); d < -2*time.Minute || d > 2*time.Minute {
				t.Errorf("list: %s modified %s", i.Key, i.Modified)
			}
			return nil
		})
	})

	run("empty object", func(t *testing.T, s blob.Store, k func(string) string) {
		mustPut(ctx, t, s, k("empty"), nil)
		if got := mustGet(ctx, t, s, k("empty")); len(got) != 0 {
			t.Fatalf("got %d bytes", len(got))
		}
		if info, err := s.Head(ctx, k("empty")); err != nil || info.Size != 0 {
			t.Fatalf("head = %+v, %v", info, err)
		}
	})

	run("large object streams", func(t *testing.T, s blob.Store, k func(string) string) {
		data := make([]byte, 9<<20+17) // over common multipart thresholds
		_, _ = rand.Read(data)
		// One byte at a time at first: the adapter must not assume Read fills the buffer.
		r := io.MultiReader(iotest.OneByteReader(bytes.NewReader(data[:100])), bytes.NewReader(data[100:]))
		if err := s.Put(ctx, k("big.bin"), r, int64(len(data))); err != nil {
			t.Fatal(err)
		}
		got := mustGet(ctx, t, s, k("big.bin"))
		if sha256.Sum256(got) != sha256.Sum256(data) {
			t.Fatalf("got %d bytes, different contents", len(got))
		}
	})

	run("missing", func(t *testing.T, s blob.Store, k func(string) string) {
		if _, err := s.Get(ctx, k("nope")); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("get missing: %v, want ErrNotFound", err)
		}
		if _, err := s.Head(ctx, k("nope")); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("head missing: %v, want ErrNotFound", err)
		}
		if err := s.Delete(ctx, k("nope")); err != nil {
			t.Fatalf("delete missing: %v, want nil", err)
		}
	})

	run("size mismatch stores nothing", func(t *testing.T, s blob.Store, k func(string) string) {
		for name, size := range map[string]int64{"short": 10, "long": 3} {
			err := s.Put(ctx, k(name), strings.NewReader("12345"), size)
			if !errors.Is(err, blob.ErrSizeMismatch) {
				t.Errorf("%s: put = %v, want ErrSizeMismatch", name, err)
			}
			if _, err := s.Head(ctx, k(name)); !errors.Is(err, blob.ErrNotFound) {
				t.Errorf("%s: an object was stored after a failed put: %v", name, err)
			}
		}
	})

	run("put again is harmless", func(t *testing.T, s blob.Store, k func(string) string) {
		mustPut(ctx, t, s, k("same"), []byte("v"))
		mustPut(ctx, t, s, k("same"), []byte("v"))
		if got := mustGet(ctx, t, s, k("same")); string(got) != "v" {
			t.Fatalf("got %q", got)
		}
	})

	run("delete", func(t *testing.T, s blob.Store, k func(string) string) {
		mustPut(ctx, t, s, k("gone"), []byte("x"))
		if err := s.Delete(ctx, k("gone")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, k("gone")); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
	})

	run("list by prefix", func(t *testing.T, s blob.Store, k func(string) string) {
		for _, key := range []string{"a/1", "a/2", "a/b/3", "ab/4", "c"} {
			mustPut(ctx, t, s, k(key), []byte(key))
		}
		list := func(prefix string) []string {
			var got []string
			if err := s.List(ctx, prefix, func(i blob.Info) error {
				got = append(got, strings.TrimPrefix(i.Key, k("")))
				if i.Size != int64(len(strings.TrimPrefix(i.Key, k("")))) {
					t.Errorf("%s: size %d", i.Key, i.Size)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			slices.Sort(got)
			return got
		}
		if got := list(k("a/")); !slices.Equal(got, []string{"a/1", "a/2", "a/b/3"}) {
			t.Fatalf("list a/ = %v (ab/4 must not match a/)", got)
		}
		if got := list(k("")); len(got) != 5 {
			t.Fatalf("list all = %v", got)
		}
		if got := list(k("zzz/")); len(got) != 0 {
			t.Fatalf("list of nothing = %v", got)
		}
	})

	run("list stops at an error", func(t *testing.T, s blob.Store, k func(string) string) {
		for i := range 5 {
			mustPut(ctx, t, s, k(fmt.Sprintf("x/%d", i)), []byte("x"))
		}
		stop := errors.New("stop")
		n := 0
		err := s.List(ctx, k("x/"), func(blob.Info) error { n++; return stop })
		if !errors.Is(err, stop) || n != 1 {
			t.Fatalf("list = %v after %d calls, want the callback's error after 1", err, n)
		}
	})

	run("invalid keys refused", func(t *testing.T, s blob.Store, _ func(string) string) {
		for _, key := range []string{"", "/abs", "a//b", "a/../b", "../x", "a/./b", "Upper", "sp ace", "a/", "tab\t", "unié", strings.Repeat("a", blob.MaxKeyLen+1)} {
			var ke *blob.KeyError
			if err := s.Put(ctx, key, strings.NewReader("x"), 1); !errors.As(err, &ke) {
				t.Errorf("put %q: %v, want KeyError", key, err)
			}
			if _, err := s.Get(ctx, key); !errors.As(err, &ke) {
				t.Errorf("get %q: %v", key, err)
			}
			if _, err := s.Head(ctx, key); !errors.As(err, &ke) {
				t.Errorf("head %q: %v", key, err)
			}
			if err := s.Delete(ctx, key); !errors.As(err, &ke) {
				t.Errorf("delete %q: %v", key, err)
			}
		}
		var ke *blob.KeyError
		if err := s.List(ctx, "../", func(blob.Info) error { return nil }); !errors.As(err, &ke) {
			t.Errorf("list ../: %v", err)
		}
	})

	run("cancelled context", func(t *testing.T, s blob.Store, k func(string) string) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.Put(cctx, k("c"), strings.NewReader("x"), 1); err == nil {
			t.Error("put with a cancelled context succeeded")
		}
		if _, err := s.Get(cctx, k("c")); err == nil {
			t.Error("get with a cancelled context succeeded")
		}
	})

	run("concurrent puts", func(t *testing.T, s blob.Store, k func(string) string) {
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b := bytes.Repeat([]byte{letters[i]}, 1000+i)
				errs <- s.Put(ctx, k(fmt.Sprintf("p/%d", i)), bytes.NewReader(b), int64(len(b)))
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		for i := range 16 {
			if got := mustGet(ctx, t, s, k(fmt.Sprintf("p/%d", i))); len(got) != 1000+i || got[0] != letters[i] {
				t.Fatalf("p/%d: %d bytes", i, len(got))
			}
		}
	})

	run("content addressing", func(t *testing.T, s blob.Store, _ func(string) string) {
		data := []byte("same contents, stored once")
		want := sha256.Sum256(data)
		sum, size, err := blob.PutContent(ctx, s, bytes.NewReader(data), t.TempDir())
		if err != nil || sum != hex.EncodeToString(want[:]) || size != int64(len(data)) {
			t.Fatalf("PutContent = %s %d %v", sum, size, err)
		}
		if got := mustGet(ctx, t, s, blob.ContentKey(sum)); !bytes.Equal(got, data) {
			t.Fatalf("stored %q", got)
		}
		again, _, err := blob.PutContent(ctx, s, bytes.NewReader(data), t.TempDir())
		if err != nil || again != sum {
			t.Fatalf("second PutContent = %s %v", again, err)
		}
	})
}

const letters = "abcdefghijklmnop"

func mustPut(ctx context.Context, t *testing.T, s blob.Store, key string, b []byte) {
	t.Helper()
	if err := s.Put(ctx, key, bytes.NewReader(b), int64(len(b))); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func mustGet(ctx context.Context, t *testing.T, s blob.Store, key string) []byte {
	t.Helper()
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
