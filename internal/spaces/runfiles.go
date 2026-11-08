// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Files nbsandboxd keeps in a run directory.
const (
	RunLog       = "log.txt"
	RunResult    = "result.json"
	RunInput     = "input.ipynb"  // the notebook as it was when the run was submitted
	RunOutput    = "output.ipynb" // the executed notebook
	RunHTML      = "output.html"
	RunValue     = "output.json" // the value the notebook returned with nbsandbox.output
	RunArtifacts = "artifacts"
	RunParams    = ".nbsandbox-params.json"
	RunStatus    = ".nbsandbox-status.json"
)

// A run's folder is mounted writable into that run, so the notebook can put
// anything there, symlinks included. Every access below goes through an
// os.Root opened at the run's own folder: nothing a run plants can make
// nbsandboxd read or write outside it.

// Owned returns nil if owner owns the space, ErrNotFound otherwise.
func (s *Service) Owned(ctx context.Context, owner, id string) error {
	_, err := s.get(ctx, owner, id)
	return err
}

// CreateRun stores the notebook snapshot and the parameters of a new run in
// object storage and returns their index entries, which the store records
// with the run's row. It runs at submission, so the run executes the
// notebook as it was then, whatever happens to the file afterwards, and any
// worker can start it.
func (s *Service) CreateRun(ctx context.Context, runID string, notebook, params []byte) ([]store.ContentRef, error) {
	if !ValidID(runID) {
		return nil, invalid("run id must be a UUID")
	}
	var refs []store.ContentRef
	for _, f := range []struct {
		name string
		data []byte
	}{{RunInput, notebook}, {RunParams, params}} {
		ref, err := s.putBytes(ctx, f.name, f.data)
		if err != nil {
			return nil, fmt.Errorf("store run %s: %w", f.name, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// putBytes stores small contents (snapshot, result) and returns its ref.
func (s *Service) putBytes(ctx context.Context, name string, data []byte) (store.ContentRef, error) {
	st, _, err := s.stage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return store.ContentRef{}, err
	}
	defer st.close()
	if err := s.putContent(ctx, st); err != nil {
		return store.ContentRef{}, err
	}
	return store.ContentRef{Path: name, SHA256: st.sha, Size: st.size}, nil
}

// PrepareRun makes the run's local folder (mounted at /run) with its
// artifacts folder, the notebook snapshot and the parameters, from object
// storage. Workers call it just before starting the run.
func (s *Service) PrepareRun(ctx context.Context, spaceID, runID string) error {
	if !ValidID(runID) {
		return invalid("run id must be a UUID")
	}
	dir := s.RunPath(spaceID, runID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(RunArtifacts, 0o700); err != nil {
		return err
	}
	for _, name := range []string{RunInput, RunParams} {
		ref, err := s.db.RunFile(ctx, runID, name)
		if err != nil {
			return fmt.Errorf("run %s: %w", name, err)
		}
		if err := s.copyOut(ctx, root, store.FileRow{Path: name, Kind: "file", SHA256: ref.SHA256, Size: ref.Size}); err != nil {
			return err
		}
	}
	s.forgetUsage(spaceID)
	return nil
}

// StoreRunOutputs uploads every regular file of the run's local folder to
// object storage, records them, and removes the folder. Links and other
// special files a run planted are skipped. Called when the run ends, before
// its final status is recorded; a run without a local folder is left alone.
// With a token, the outputs are only recorded while the worker holds that
// lease (store.ErrFenced otherwise).
func (s *Service) StoreRunOutputs(ctx context.Context, spaceID, runID string, token int64) error {
	if !ValidID(runID) {
		return ErrNotFound
	}
	dir := s.RunPath(spaceID, runID)
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var refs []store.ContentRef
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		// Not stored: temporaries, and the events file (its contents are
		// recorded as events).
		if err != nil || !d.Type().IsRegular() || strings.Contains(path.Base(p), internalPrefix+"-tmp-") || p == internalPrefix+"-events.jsonl" {
			return nil
		}
		f, err := root.Open(p)
		if err != nil {
			return nil
		}
		st, _, err := s.stage(f, 1<<62)
		_ = f.Close()
		if err != nil {
			return err
		}
		defer st.close()
		if err := s.putContent(ctx, st); err != nil {
			return err
		}
		refs = append(refs, store.ContentRef{Path: p, SHA256: st.sha, Size: st.size})
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.db.PutRunFiles(ctx, runID, token, refs); err != nil {
		_ = root.Close()
		_ = os.RemoveAll(dir) // this worker's copy either way
		return err
	}
	_ = root.Close()
	if err := os.RemoveAll(dir); err != nil {
		s.log.Warn("remove stored run folder", "run_id", runID, "error", err)
	}
	s.forgetUsage(spaceID)
	return nil
}

// localRun opens the run's local folder if it is there (the run is active
// on this host, or finishing).
func (s *Service) localRun(spaceID, runID string) (*os.Root, bool) {
	if !ValidID(runID) {
		return nil, false
	}
	root, err := os.OpenRoot(s.RunPath(spaceID, runID))
	return root, err == nil
}

// OpenRunLog creates log.txt in the run's local folder, exclusively, and
// returns it open for writing: a run is executed at most once.
func (s *Service) OpenRunLog(spaceID, runID string) (io.WriteCloser, error) {
	root, ok := s.localRun(spaceID, runID)
	if !ok {
		return nil, fmt.Errorf("create log: the run's folder is not prepared")
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(RunLog, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create log: %w", err)
	}
	return f, nil
}

// ReadRunFile reads a small file of a run (status, result, output). Anything
// larger than max is an error; a missing file is ErrNotFound.
func (s *Service) ReadRunFile(ctx context.Context, spaceID, runID, name string, max int64) ([]byte, error) {
	f, info, err := s.OpenRunFile(ctx, spaceID, runID, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info.Size() > max {
		return nil, &LimitError{Msg: fmt.Sprintf("%s is larger than %d bytes", name, max)}
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// RunFileSize returns the size of a run's file.
func (s *Service) RunFileSize(ctx context.Context, spaceID, runID, name string) (int64, bool) {
	if root, ok := s.localRun(spaceID, runID); ok {
		defer func() { _ = root.Close() }()
		if info, err := root.Lstat(name); err == nil && info.Mode().IsRegular() {
			return info.Size(), true
		}
		return 0, false
	}
	ref, err := s.db.RunFile(ctx, runID, name)
	return ref.Size, err == nil
}

// WriteRunFile replaces a small file of a run: in its local folder while
// that exists, otherwise in object storage (a run cancelled before it
// started, or a result rewritten after the outputs were stored).
func (s *Service) WriteRunFile(ctx context.Context, spaceID, runID, name string, data []byte) error {
	if !ValidID(runID) {
		return ErrNotFound
	}
	if root, ok := s.localRun(spaceID, runID); ok {
		defer func() { _ = root.Close() }()
		tmp := internalPrefix + "-tmp-" + NewID()
		if err := root.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		if err := root.Rename(tmp, name); err != nil {
			_ = root.Remove(tmp)
			return err
		}
		return nil
	}
	ref, err := s.putBytes(ctx, name, data)
	if err != nil {
		return err
	}
	return s.db.PutRunFiles(ctx, runID, 0, []store.ContentRef{ref})
}

// OpenRunFile opens a run's output (output.ipynb, output.html, log.txt) or
// an artifact ("artifacts/<path>"): from the local folder while the run is
// active here, from object storage afterwards. Only regular files;
// ErrNotFound otherwise.
func (s *Service) OpenRunFile(ctx context.Context, spaceID, runID, rel string) (*os.File, os.FileInfo, error) {
	if root, ok := s.localRun(spaceID, runID); ok {
		defer func() { _ = root.Close() }()
		return openRegular(root, rel)
	}
	if !ValidID(runID) {
		return nil, nil, ErrNotFound
	}
	ref, err := s.db.RunFile(ctx, runID, rel)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	f, err := s.fetch(ctx, ref.SHA256, ref.Size)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, namedInfo{FileInfo: info, name: path.Base(rel)}, nil
}

// TempFile copies r into an unlinked temporary file, positioned at the start,
// that reports name as its name.
func (s *Service) TempFile(name string, r io.Reader) (*os.File, os.FileInfo, error) {
	if err := os.MkdirAll(s.tmpDir(), 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.CreateTemp(s.tmpDir(), "tmp-*")
	if err != nil {
		return nil, nil, err
	}
	_ = os.Remove(f.Name()) // readable until closed
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, namedInfo{FileInfo: info, name: name}, nil
}

// namedInfo gives a fetched temporary file its run file's name.
type namedInfo struct {
	os.FileInfo
	name string
}

func (n namedInfo) Name() string { return n.name }

// openRegular opens rel inside root if it is a regular file (never a symlink
// or folder).
func openRegular(root *os.Root, rel string) (*os.File, os.FileInfo, error) {
	info, err := root.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, ErrNotFound
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}

// ValidateArtifactPath checks a path relative to a run's artifacts folder.
func ValidateArtifactPath(p string) error { return ValidatePath(p) }

// ListRunArtifacts lists a run's artifacts, with paths relative to the
// artifacts folder.
func (s *Service) ListRunArtifacts(ctx context.Context, spaceID, runID string) ([]File, error) {
	if root, ok := s.localRun(spaceID, runID); ok {
		defer func() { _ = root.Close() }()
		var out []File
		_ = fs.WalkDir(root.FS(), RunArtifacts, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == RunArtifacts {
					return fs.SkipAll
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if info, err := d.Info(); err == nil {
				out = append(out, File{Path: strings.TrimPrefix(p, RunArtifacts+"/"), Size: info.Size(), Modified: info.ModTime().UTC()})
			}
			return nil
		})
		sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
		return out, nil
	}
	refs, err := s.db.RunFiles(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []File
	for _, r := range refs {
		if strings.HasPrefix(r.Path, RunArtifacts+"/") {
			out = append(out, File{Path: strings.TrimPrefix(r.Path, RunArtifacts+"/"), Size: r.Size})
		}
	}
	return out, nil
}

// RemoveRun deletes a run's local folder, if any. Its recorded files go with
// its row; their contents with the cleanup.
func (s *Service) RemoveRun(spaceID, runID string) error {
	if !ValidID(runID) {
		return ErrNotFound
	}
	if err := os.RemoveAll(s.RunPath(spaceID, runID)); err != nil {
		return err
	}
	s.forgetUsage(spaceID)
	return nil
}

// ReadNotebook reads and validates the notebook a run will execute.
func (s *Service) ReadNotebook(ctx context.Context, spaceID, p string) ([]byte, error) {
	if err := ValidatePath(p); err != nil {
		return nil, err
	}
	if !strings.HasSuffix(strings.ToLower(p), ".ipynb") {
		return nil, invalid("%s is not a notebook (.ipynb)", p)
	}
	row, err := s.db.GetFile(ctx, spaceID, p)
	if err != nil || row.Kind != "file" {
		return nil, invalid("notebook %s does not exist in the space", p)
	}
	if row.Size > s.cfg.MaxFile {
		return nil, invalid("notebook %s is larger than the file limit", p)
	}
	b, err := s.readContent(ctx, row.SHA256, row.Size)
	if err != nil {
		return nil, err
	}
	if err := ValidateNotebook(strings.NewReader(string(b))); err != nil {
		return nil, invalid("%s: %v", p, err)
	}
	return b, nil
}

// ReadSpaceFile reads a small user file, if it exists (e.g. requirements.txt).
func (s *Service) ReadSpaceFile(spaceID, p string, max int64) ([]byte, bool) {
	ctx := context.Background()
	row, err := s.db.GetFile(ctx, spaceID, p)
	if err != nil || row.Kind != "file" || row.Size > max {
		return nil, false
	}
	b, err := s.readContent(ctx, row.SHA256, max)
	return b, err == nil
}

// SpaceEntryExists reports whether a file or folder exists.
func (s *Service) SpaceEntryExists(spaceID, p string) bool {
	if ValidatePath(p) != nil {
		return false
	}
	_, err := s.db.GetFile(context.Background(), spaceID, p)
	return err == nil
}

// CheckConsistency runs at startup: it finishes deletes that a
// crash interrupted (.trash-* folders and space folders without a database
// row) and logs rows whose folder is missing.
func (s *Service) CheckConsistency(ctx context.Context) error {
	ids, err := s.db.SpaceIDs(ctx)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(s.spacesDir())
	if err != nil {
		return err
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(s.spacesDir(), name)
		switch {
		case strings.HasPrefix(name, ".trash-"):
			s.log.Info("removing leftover deleted space", "dir", name)
			if err := os.RemoveAll(full); err != nil {
				s.log.Warn("remove leftover", "dir", name, "error", err)
			}
		case e.IsDir() && ValidID(name) && !ids[name]:
			s.log.Warn("space folder without a database row; removing it", "space_id", name)
			trash := filepath.Join(s.spacesDir(), ".trash-"+name)
			if err := os.Rename(full, trash); err == nil {
				_ = os.RemoveAll(trash)
			}
		case e.IsDir() && ValidID(name):
			onDisk[name] = true
			s.removeWorkingCopies(name)
		}
	}
	for id := range ids {
		if !onDisk[id] {
			// The folder holds only run outputs, bundles and working copies:
			// recreate it so runs can start.
			s.log.Warn("space has a database row but no folder on this host; recreating it", "space_id", id)
			for _, sub := range []string{RunsDir, WorkDir} {
				if err := os.MkdirAll(filepath.Join(s.Dir(id), sub), 0o700); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
