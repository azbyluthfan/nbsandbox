// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package tarfs

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Whatever a run's Pod sends back as a tar stream, nothing lands outside the
// folder, nothing but regular files and folders is created, and no more
// than the budget is written.
func FuzzReadTar(f *testing.F) {
	seed := func(entries ...tar.Header) []byte {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for _, h := range entries {
			_ = tw.WriteHeader(&h)
			if h.Typeflag == tar.TypeReg {
				_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			}
		}
		_ = tw.Close()
		return b.Bytes()
	}
	f.Add(seed(tar.Header{Typeflag: tar.TypeReg, Name: "a.txt", Size: 3, Mode: 0o644}))
	f.Add(seed(tar.Header{Typeflag: tar.TypeReg, Name: "../escape", Size: 1, Mode: 0o644}))
	f.Add(seed(tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc/passwd"},
		tar.Header{Typeflag: tar.TypeReg, Name: "link/x", Size: 1}))
	f.Add(seed(tar.Header{Typeflag: tar.TypeDir, Name: "d/"}, tar.Header{Typeflag: tar.TypeReg, Name: "d/b", Size: 5}))
	f.Add(seed(tar.Header{Typeflag: tar.TypeReg, Name: "/abs", Size: 1}))
	base := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		// A folder per input, removed when it's done.
		parent, err := os.MkdirTemp(base, "in")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(parent) }()
		dir := filepath.Join(parent, "run")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		const budget = 4 << 10
		_, _ = Read(bytes.NewReader(data), dir, budget)
		if entries, _ := os.ReadDir(parent); len(entries) != 1 {
			t.Fatalf("wrote outside the folder: %v", entries)
		}
		var total int64
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() && !info.IsDir() {
				t.Fatalf("created %s (%v)", p, info.Mode())
			}
			if info.Mode().IsRegular() {
				total += info.Size()
			}
			return nil
		})
		if total > budget {
			t.Fatalf("wrote %d bytes, budget %d", total, budget)
		}
	})
}
