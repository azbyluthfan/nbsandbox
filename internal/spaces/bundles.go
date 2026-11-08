// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Function names: lowercase letters, digits, - and _, starting with a letter
// or digit; they appear in URLs.
var functionNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidFunctionName reports whether name can name a function.
func ValidFunctionName(name string) bool { return functionNameRE.MatchString(name) }

// MaxBundleFiles bounds how many files one published version may hold.
const MaxBundleFiles = 10000

// BundleFiles returns what a new function version holds: the notebook and
// the listed files and folders (everything in them), at their paths in the
// space, as index entries. Nothing is copied: the version refers to the same
// contents in object storage, and later edits to the space don't change it.
// The version counts toward the space's quota. It returns the entries and
// their total size.
func (s *Service) BundleFiles(ctx context.Context, spaceID, notebook string, files []string) ([]store.ContentRef, int64, error) {
	paths := append([]string{notebook}, files...)
	for _, p := range paths {
		if err := ValidatePath(p); err != nil {
			return nil, 0, err
		}
	}
	usage, err := s.Usage(ctx, spaceID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.ListFiles(ctx, spaceID)
	if err != nil {
		return nil, 0, err
	}
	byPath := map[string]store.FileRow{}
	for _, r := range rows {
		byPath[r.Path] = r
	}
	var out []store.ContentRef
	seen := map[string]bool{}
	var total int64
	add := func(r store.FileRow) error {
		if seen[r.Path] {
			return nil // listed twice
		}
		seen[r.Path] = true
		if len(out) >= MaxBundleFiles {
			return &LimitError{Msg: fmt.Sprintf("a function version may hold at most %d files", MaxBundleFiles)}
		}
		total += r.Size
		if usage.BytesUsed+total > usage.QuotaBytes {
			return &LimitError{Msg: fmt.Sprintf("publishing would exceed the space quota of %d bytes (%d used)", usage.QuotaBytes, usage.BytesUsed)}
		}
		out = append(out, store.ContentRef{Path: r.Path, SHA256: r.SHA256, Size: r.Size})
		return nil
	}
	for i, p := range paths {
		r, found := byPath[p]
		switch {
		case !found:
			return nil, 0, invalid("%s does not exist in the space", p)
		case i == 0 && r.Kind != "file":
			return nil, 0, invalid("%s is not a notebook file", p)
		case r.Kind == "file":
			if err := add(r); err != nil {
				return nil, 0, err
			}
		default: // a folder: everything in it
			for _, q := range rows {
				if q.Kind == "file" && strings.HasPrefix(q.Path, p+"/") {
					if err := add(q); err != nil {
						return nil, 0, err
					}
				}
			}
		}
	}
	return out, total, nil
}

// ReadBundleNotebook reads a published version's notebook.
func (s *Service) ReadBundleNotebook(ctx context.Context, spaceID, function string, version int, notebook string) ([]byte, error) {
	if !ValidFunctionName(function) || ValidatePath(notebook) != nil {
		return nil, ErrNotFound
	}
	refs, err := s.db.FunctionVersionFiles(ctx, spaceID, function, version)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if r.Path == notebook {
			return s.readContent(ctx, r.SHA256, min(r.Size, s.cfg.MaxFile+1))
		}
	}
	return nil, ErrNotFound
}

// ContentOf reads small contents by SHA-256 (at most limit bytes).
func (s *Service) ContentOf(ctx context.Context, sha string, limit int64) ([]byte, error) {
	return s.readContent(ctx, sha, limit)
}

// CheckoutBundle writes a published version's files into the run's working
// copy (mounted read-only at /space for the invocation) and returns its path.
func (s *Service) CheckoutBundle(ctx context.Context, spaceID, function string, version int, runID string) (string, error) {
	if !ValidID(runID) || !ValidFunctionName(function) {
		return "", invalid("invalid run id or function")
	}
	refs, err := s.db.FunctionVersionFiles(ctx, spaceID, function, version)
	if err != nil {
		return "", err
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("version %d of %s has no files", version, function)
	}
	dir := s.WorkPath(spaceID, runID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create working copy: %w", err)
	}
	fail := func(err error) (string, error) {
		s.Discard(spaceID, runID)
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = root.Close() }()
	co := checkout{Files: map[string]string{}}
	for _, r := range refs {
		if d := path.Dir(r.Path); d != "." {
			if err := root.MkdirAll(d, 0o700); err != nil {
				return fail(err)
			}
		}
		if err := s.copyOut(ctx, root, store.FileRow{Path: r.Path, Kind: "file", SHA256: r.SHA256, Size: r.Size}); err != nil {
			return fail(fmt.Errorf("check out %s: %w", r.Path, err))
		}
		co.Files[r.Path] = r.SHA256
		co.Bytes += r.Size // already counted as the published version
	}
	b, _ := json.Marshal(co)
	if err := os.WriteFile(s.checkoutMeta(spaceID, runID), b, 0o600); err != nil {
		return fail(err)
	}
	s.forgetUsage(spaceID)
	return dir, nil
}
