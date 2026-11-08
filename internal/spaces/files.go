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
	"os"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// UploadPart is one file in an upload request.
type UploadPart struct {
	Name string    // file name only (no folders)
	Body io.Reader // the content
}

// NextPart returns the next part, or io.EOF when there are no more.
type NextPart func() (*UploadPart, error)

// Entry types in file listings.
const (
	TypeFile   = "file"
	TypeFolder = "folder"
)

// File describes a stored file or folder.
type File struct {
	Path     string    `json:"path"`
	Type     string    `json:"type,omitempty"` // file | folder (listings of user files)
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	// Version identifies the contents: the SHA-256 of a file's bytes, so it
	// changes exactly when the contents do (user files).
	Version string `json:"version,omitempty"`
}

func fileFromRow(f store.FileRow) File {
	if f.Kind == "folder" {
		return File{Path: f.Path, Type: TypeFolder, Modified: f.ModifiedAt.UTC()}
	}
	return File{Path: f.Path, Type: TypeFile, Size: f.Size, Modified: f.ModifiedAt.UTC(), Version: f.SHA256}
}

// staged is a file's contents written to a temporary file while being
// hashed and counted, before it goes to object storage.
type staged struct {
	tmp  *os.File
	sha  string
	size int64
}

func (st *staged) close() {
	if st.tmp != nil {
		_ = st.tmp.Close()
		_ = os.Remove(st.tmp.Name())
	}
}

// stage reads r into a temporary file, up to limit bytes; more than limit is
// reported as tooLarge (the rest is not read).
func (s *Service) stage(r io.Reader, limit int64) (st *staged, tooLarge bool, err error) {
	f, err := os.CreateTemp(s.tmpDir(), "stage-*")
	if err != nil {
		return nil, false, err
	}
	st = &staged{tmp: f}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if err != nil {
		st.close()
		return nil, false, err
	}
	if n > limit {
		st.close()
		return nil, true, nil
	}
	st.sha, st.size = hex.EncodeToString(h.Sum(nil)), n
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		st.close()
		return nil, false, err
	}
	return st, false, nil
}

// putContent stores staged contents in object storage (once per SHA-256).
func (s *Service) putContent(ctx context.Context, st *staged) error {
	key := blob.ContentKey(st.sha)
	// Already there and recent: nothing to do. Older copies are written
	// again, which refreshes their time, so cleanup can't remove contents
	// this change is about to record (see blob.RefreshAfter).
	if info, err := s.blobs.Head(ctx, key); err == nil && info.Size == st.size && time.Since(info.Modified) < blob.RefreshAfter {
		return nil
	}
	if _, err := st.tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return s.blobs.Put(ctx, key, st.tmp, st.size)
}

// mapStoreErr turns file index errors into the domain's.
func (s *Service) mapStoreErr(p string, err error) error {
	var vm *store.VersionMismatch
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrExists):
		return ErrExists
	case errors.Is(err, store.ErrNotEmpty):
		return ErrNotEmpty
	case errors.Is(err, store.ErrIsDir):
		return invalid("%s already exists and is not a regular file", p)
	case errors.Is(err, store.ErrNotDir):
		return invalid("a folder above %s is a file", p)
	case errors.Is(err, store.ErrQuota):
		return &LimitError{Msg: "the change would exceed the space quota"}
	case errors.As(err, &vm):
		return &VersionError{Current: vm.Current}
	}
	return err
}

// Upload stores every part under dir ("" = top level) in owner's space, all
// or nothing: each part is read with the per-file and per-space limits
// enforced as bytes arrive, notebooks are validated, the contents go to
// object storage, and only when every part is good does the file index
// change, in one transaction. Existing files are replaced.
func (s *Service) Upload(ctx context.Context, owner, id, dir string, next NextPart) ([]File, error) {
	if err := ValidateDir(dir); err != nil {
		return nil, err
	}
	if _, err := s.get(ctx, owner, id); err != nil {
		return nil, err
	}
	used, err := s.Usage(ctx, id)
	if err != nil {
		return nil, err
	}
	var parts []*staged
	var dests []string
	defer func() {
		for _, p := range parts {
			p.close()
		}
	}()
	seen := map[string]bool{}
	var incoming int64
	for {
		part, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(parts) >= s.cfg.MaxFilesPerUpload {
			return nil, &LimitError{Msg: fmt.Sprintf("at most %d files per upload", s.cfg.MaxFilesPerUpload)}
		}
		if part.Name == "" || strings.Contains(part.Name, "/") {
			return nil, invalid("each part needs a plain file name (use ?folder= for folders), got %q", part.Name)
		}
		dest := part.Name
		if dir != "" {
			dest = dir + "/" + part.Name
		}
		if err := ValidatePath(dest); err != nil {
			return nil, err
		}
		if seen[dest] {
			return nil, invalid("%s appears twice in one upload", dest)
		}
		seen[dest] = true
		budget := min(s.cfg.MaxFile, used.QuotaBytes-used.BytesUsed-incoming)
		st, tooLarge, err := s.stage(part.Body, max(budget, 0))
		switch {
		case err != nil:
			return nil, err
		case tooLarge && budget >= s.cfg.MaxFile:
			return nil, &LimitError{Msg: fmt.Sprintf("%s is larger than the %d-byte file limit", dest, s.cfg.MaxFile)}
		case tooLarge:
			return nil, &LimitError{Msg: fmt.Sprintf("upload would exceed the space quota of %d bytes (%d used)", used.QuotaBytes, used.BytesUsed)}
		}
		parts = append(parts, st)
		dests = append(dests, dest)
		incoming += st.size
		if strings.HasSuffix(strings.ToLower(dest), ".ipynb") {
			if err := ValidateNotebook(st.tmp); err != nil {
				return nil, invalid("%s: %v", dest, err)
			}
		}
	}
	if len(parts) == 0 {
		return nil, invalid("no files in the upload")
	}
	writes := make([]store.FileWrite, len(parts))
	for i, st := range parts {
		if err := s.putContent(ctx, st); err != nil {
			return nil, err
		}
		writes[i] = store.FileWrite{Path: dests[i], SHA256: st.sha, Size: st.size}
	}
	if err := s.db.PutFiles(ctx, id, writes, s.now(), s.fileQuota(ctx, id)); err != nil {
		return nil, s.mapStoreErr(dests[0], err)
	}
	s.forgetUsage(id)
	out := make([]File, 0, len(parts))
	for _, d := range dests {
		f, err := s.db.GetFile(ctx, id, d)
		if err != nil {
			return nil, err
		}
		out = append(out, fileFromRow(f))
	}
	return out, nil
}

// ValidateNotebook checks r holds a JSON notebook with nbformat 4 and a cells
// list.
func ValidateNotebook(r io.Reader) error {
	var nb struct {
		NBFormat *int              `json:"nbformat"`
		Cells    []json.RawMessage `json:"cells"`
	}
	dec := json.NewDecoder(r)
	if err := dec.Decode(&nb); err != nil {
		return errors.New("not a valid notebook: the file is not JSON")
	}
	if nb.NBFormat == nil {
		return errors.New("not a valid notebook: nbformat is missing")
	}
	if *nb.NBFormat != 4 {
		return fmt.Errorf("not a supported notebook: nbformat %d (only 4 is supported)", *nb.NBFormat)
	}
	if nb.Cells == nil {
		return errors.New("not a valid notebook: cells list is missing")
	}
	return nil
}

// ListFiles returns the space's files and folders, sorted by path.
func (s *Service) ListFiles(ctx context.Context, owner, id string) ([]File, error) {
	if _, err := s.get(ctx, owner, id); err != nil {
		return nil, err
	}
	rows, err := s.db.ListFiles(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(rows))
	for _, r := range rows {
		out = append(out, fileFromRow(r))
	}
	return out, nil
}

// OpenFile opens a file for download: its contents, copied from object
// storage into an unlinked temporary file (so it can be served with
// ranges), and its index entry. Folders and missing paths are ErrNotFound.
// The caller closes the file.
func (s *Service) OpenFile(ctx context.Context, owner, id, p string) (*os.File, File, error) {
	if err := ValidatePath(p); err != nil {
		return nil, File{}, err
	}
	if _, err := s.get(ctx, owner, id); err != nil {
		return nil, File{}, err
	}
	row, err := s.db.GetFile(ctx, id, p)
	if err != nil || row.Kind != "file" {
		return nil, File{}, ErrNotFound
	}
	f, err := s.fetch(ctx, row.SHA256, row.Size)
	if err != nil {
		return nil, File{}, err
	}
	return f, fileFromRow(row), nil
}

// fetch copies contents from object storage into an unlinked temporary file,
// positioned at the start.
func (s *Service) fetch(ctx context.Context, sha string, size int64) (*os.File, error) {
	rc, err := s.blobs.Get(ctx, blob.ContentKey(sha))
	if errors.Is(err, blob.ErrNotFound) {
		s.log.Error("file contents missing from object storage", "sha256", sha)
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	f, err := os.CreateTemp(s.tmpDir(), "fetch-*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(f.Name()) // the open file stays readable until closed
	if n, err := io.Copy(f, io.LimitReader(rc, size+1)); err != nil || n != size {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("object storage returned %d bytes, the index says %d", n, size)
		}
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// readContent reads small contents fully (at most limit bytes).
func (s *Service) readContent(ctx context.Context, sha string, limit int64) ([]byte, error) {
	rc, err := s.blobs.Get(ctx, blob.ContentKey(sha))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, limit))
}

// DeleteFile removes one file or an empty folder.
func (s *Service) DeleteFile(ctx context.Context, owner, id, p string) error {
	return s.DeletePath(ctx, owner, id, p, false)
}
