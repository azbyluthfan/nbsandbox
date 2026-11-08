// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package tarfs copies folders as tar streams safely: only regular files and
// folders, every path checked and written inside the target folder
// (os.Root), within a size budget. The Kubernetes backend copies run files
// in and out of Pods with it.
package tarfs

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// Write writes the regular files and folders under dir to w as a tar
// stream, with paths relative to dir. Links, special files and the
// top-level files named in skip are left out.
func Write(w io.Writer, dir string, skip ...string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tw := tar.NewWriter(w)
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		if slices.Contains(skip, p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: 0o755, ModTime: info.ModTime()})
		case d.Type().IsRegular():
			f, err := root.Open(p)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, info.Size())
			return err
		default:
			return nil // links and special files
		}
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// ErrTooLarge: the files in the stream are larger than allowed.
var ErrTooLarge = errors.New("the run's files are larger than allowed")

// Read extracts a tar stream into dir: regular files and folders only,
// every path checked and written inside dir (os.Root), at most maxBytes in
// total. It returns the paths it skipped (links and special files the run
// made), which never reach the host.
func Read(r io.Reader, dir string, maxBytes int64) (skipped []string, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return skipped, nil
		}
		if err != nil {
			return skipped, err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." || name == "" {
			continue
		}
		if !fs.ValidPath(name) {
			return skipped, fmt.Errorf("unsafe path in the run's files: %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return skipped, err
			}
		case tar.TypeReg:
			if h.Size < 0 || h.Size > maxBytes-total { // compared before adding: no overflow
				return skipped, ErrTooLarge
			}
			total += h.Size
			if d := path.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o700); err != nil {
					return skipped, err
				}
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return skipped, err
			}
			_, err = io.CopyN(f, tr, h.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return skipped, err
			}
			_ = root.Chtimes(name, h.ModTime, h.ModTime)
		default:
			skipped = append(skipped, name)
		}
	}
}

// ClearDir removes everything inside dir, keeping dir.
func ClearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, e := range entries {
		if err := root.RemoveAll(e.Name()); err != nil {
			return err
		}
	}
	return nil
}
