// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// A run works on a copy of the space: Checkout writes the space's files into
// work/<run>/ before the run starts, the run sees that folder at /space, and
// for a run that may change the space, Checkin compares the folder with what
// was checked out and writes the differences back, each checked against the
// version the run started from. Files edited meanwhile keep the edit; the
// run's version is kept beside them (store.ConflictPath).

// checkout records what a working copy started from (work/<run>.json,
// beside the folder, so the run can't see or change it).
type checkout struct {
	Files   map[string]string `json:"files"` // path -> SHA-256
	Folders []string          `json:"folders"`
	Bytes   int64             `json:"bytes"`
}

func (s *Service) checkoutMeta(spaceID, runID string) string {
	return filepath.Join(s.Dir(spaceID), WorkDir, runID+".json")
}

// Checkout writes the space's files into the run's working copy and returns
// its path. The run's own outputs go elsewhere (RunPath).
func (s *Service) Checkout(ctx context.Context, spaceID, runID string) (string, error) {
	if !ValidID(runID) {
		return "", invalid("run id must be a UUID")
	}
	dir := s.WorkPath(spaceID, runID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create working copy: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			s.Discard(spaceID, runID)
		}
	}()
	rows, err := s.db.ListFiles(ctx, spaceID)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	co := checkout{Files: map[string]string{}}
	for _, r := range rows {
		if r.Kind == "folder" {
			co.Folders = append(co.Folders, r.Path)
			if err := root.MkdirAll(r.Path, 0o700); err != nil {
				return "", err
			}
			continue
		}
		if dir := path.Dir(r.Path); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
		}
		if err := s.copyOut(ctx, root, r); err != nil {
			return "", fmt.Errorf("check out %s: %w", r.Path, err)
		}
		co.Files[r.Path] = r.SHA256
		co.Bytes += r.Size
	}
	b, _ := json.Marshal(co)
	if err := os.WriteFile(s.checkoutMeta(spaceID, runID), b, 0o600); err != nil {
		return "", err
	}
	ok = true
	s.forgetUsage(spaceID)
	return dir, nil
}

func (s *Service) copyOut(ctx context.Context, root *os.Root, r store.FileRow) error {
	rc, err := s.blobs.Get(ctx, blob.ContentKey(r.SHA256))
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	f, err := root.OpenFile(r.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(rc, r.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != r.Size {
		err = fmt.Errorf("object storage returned %d bytes, the index says %d", n, r.Size)
	}
	return err
}

// Checkin writes a finished run's changes back to the space: files it
// created or changed, files and empty folders it removed, folders it made.
// Links and anything that isn't a regular file or folder are ignored, as are
// names the space can't hold (reported in skipped). Edits made meanwhile win
// (see Conflict). If the changes would exceed the quota, nothing is applied
// (store.ErrQuota is returned).
func (s *Service) Checkin(ctx context.Context, spaceID, runID string, token int64) (conflicts []store.Conflict, skipped []string, err error) {
	b, err := os.ReadFile(s.checkoutMeta(spaceID, runID))
	if err != nil {
		return nil, nil, fmt.Errorf("working copy record: %w", err)
	}
	var co checkout
	if err := json.Unmarshal(b, &co); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(s.WorkPath(spaceID, runID))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()

	seenFiles, seenFolders := map[string]bool{}, map[string]bool{}
	var writes []store.FileWrite
	var newFolders []string
	var staged []*staged
	defer func() {
		for _, st := range staged {
			st.close()
		}
	}()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), internalPrefix) || ValidatePath(p) != nil {
			skipped = append(skipped, p)
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			seenFolders[p] = true
			if !hasString(co.Folders, p) {
				newFolders = append(newFolders, p)
			}
		case d.Type().IsRegular():
			seenFiles[p] = true
			sum, err := hashIn(root, p)
			if err != nil {
				return err
			}
			if co.Files[p] == sum {
				return nil // unchanged
			}
			f, err := root.Open(p)
			if err != nil {
				return err
			}
			st, _, err := s.stage(f, 1<<62)
			_ = f.Close()
			if err != nil {
				return err
			}
			staged = append(staged, st)
			if err := s.putContent(ctx, st); err != nil {
				return err
			}
			expect := co.Files[p] // "" when the run created it
			writes = append(writes, store.FileWrite{Path: p, SHA256: st.sha, Size: st.size, If: &expect})
		default:
			skipped = append(skipped, p) // a link, socket, ...: not stored
		}
		return nil
	})
	if err != nil {
		return nil, skipped, err
	}
	var deletes []store.FileDelete
	for p, sum := range co.Files {
		if !seenFiles[p] {
			deletes = append(deletes, store.FileDelete{Path: p, If: sum})
		}
	}
	gone := []string{}
	for _, f := range co.Folders {
		if !seenFolders[f] {
			gone = append(gone, f)
		}
	}
	sort.Strings(gone) // parents before children; the store removes the deepest first
	for _, f := range gone {
		deletes = append(deletes, store.FileDelete{Path: f, Folder: true})
	}
	if len(writes) == 0 && len(deletes) == 0 && len(newFolders) == 0 {
		return nil, skipped, nil
	}
	conflicts, err = s.db.ApplyRunChanges(ctx, spaceID, writes, deletes, newFolders, runID[:8], s.now(), s.fileQuota(ctx, spaceID), lease(runID, token))
	s.forgetUsage(spaceID)
	return conflicts, skipped, err
}

func hasString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func hashIn(root *os.Root, p string) (string, error) {
	f, err := root.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Discard removes a run's working copy.
func (s *Service) Discard(spaceID, runID string) {
	if !ValidID(runID) {
		return
	}
	if err := os.RemoveAll(s.WorkPath(spaceID, runID)); err != nil {
		s.log.Warn("remove working copy", "space_id", spaceID, "run_id", runID, "error", err)
	}
	_ = os.Remove(s.checkoutMeta(spaceID, runID))
	s.forgetUsage(spaceID)
}

// checkedOutBytes is the size of the files the space's working copies
// started from: already counted in the file index, so not counted twice.
func (s *Service) checkedOutBytes(id string) int64 {
	metas, err := filepath.Glob(filepath.Join(s.Dir(id), WorkDir, "*.json"))
	if err != nil {
		return 0
	}
	var n int64
	for _, m := range metas {
		b, err := os.ReadFile(m) //nolint:gosec // our own file under the space's folder
		if err != nil {
			continue
		}
		var co checkout
		if json.Unmarshal(b, &co) == nil {
			n += co.Bytes
		}
	}
	return n
}

// removeWorkingCopies removes every working copy of a space (at startup: no
// run is active then, and runs that were are finished as interrupted).
func (s *Service) removeWorkingCopies(id string) {
	dir := filepath.Join(s.Dir(id), WorkDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("list working copies", "space_id", id, "error", err)
		}
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	if len(entries) > 0 {
		s.log.Info("removed working copies left by a restart", "space_id", id, "count", len(entries))
	}
}

// lease is the fence for a worker's writes for a run (none for token 0).
func lease(runID string, token int64) store.Lease {
	if token == 0 {
		return store.Lease{}
	}
	return store.Lease{RunID: runID, Token: token}
}
