// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// ErrExists: the target of a create or move already exists (409).
var ErrExists = errors.New("already exists")

// ErrNotEmpty: a folder still has files and recursive deletion wasn't asked
// for (409).
var ErrNotEmpty = errors.New("the folder is not empty; delete it with recursive=true")

// VersionError is a failed precondition on a file's version: the caller
// edited an older version (412), or didn't say which version it is
// replacing (428).
type VersionError struct {
	Current  string // the file's version now ("" if it doesn't exist)
	Required bool   // If-Match was missing
}

func (e *VersionError) Error() string {
	if e.Required {
		return "replacing a file needs If-Match with the version you are replacing (or If-None-Match: * to create one)"
	}
	if e.Current == "" {
		return "the file no longer exists"
	}
	return "the file has changed since that version; reload it, then save again"
}

// WriteRequest is one file save.
type WriteRequest struct {
	Path        string
	Body        io.Reader
	IfMatch     string // the version being replaced; "" = none given
	IfNoneMatch bool   // create only: fail if the file exists
}

// WriteFile creates or replaces one file, with the same limits and checks as
// an upload. Replacing needs the version the caller is replacing, so two
// editors can't silently overwrite each other; the check and the change are
// one transaction in the file index.
func (s *Service) WriteFile(ctx context.Context, owner, id string, req WriteRequest) (File, error) {
	if err := ValidatePath(req.Path); err != nil {
		return File{}, err
	}
	if _, err := s.get(ctx, owner, id); err != nil {
		return File{}, err
	}
	cur, err := s.db.GetFile(ctx, id, req.Path)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return File{}, err
	}
	var replacing int64
	switch {
	case exists && cur.Kind == "folder":
		return File{}, invalid("%s exists and is not a regular file", req.Path)
	case exists && req.IfNoneMatch:
		return File{}, &VersionError{Current: cur.SHA256}
	case exists && req.IfMatch == "":
		return File{}, &VersionError{Current: cur.SHA256, Required: true}
	case exists && req.IfMatch != cur.SHA256:
		return File{}, &VersionError{Current: cur.SHA256}
	case exists:
		replacing = cur.Size
	case req.IfMatch != "":
		return File{}, &VersionError{}
	}
	used, err := s.Usage(ctx, id)
	if err != nil {
		return File{}, err
	}
	budget := min(s.cfg.MaxFile, used.QuotaBytes-used.BytesUsed+replacing)
	st, tooLarge, err := s.stage(req.Body, max(budget, 0))
	switch {
	case err != nil:
		return File{}, err
	case tooLarge && budget >= s.cfg.MaxFile:
		return File{}, &LimitError{Msg: fmt.Sprintf("%s is larger than the %d-byte file limit", req.Path, s.cfg.MaxFile)}
	case tooLarge:
		return File{}, &LimitError{Msg: fmt.Sprintf("the file would exceed the space quota of %d bytes (%d used)", used.QuotaBytes, used.BytesUsed)}
	}
	defer st.close()
	if strings.HasSuffix(strings.ToLower(req.Path), ".ipynb") {
		if err := ValidateNotebook(st.tmp); err != nil {
			return File{}, invalid("%s: %v", req.Path, err)
		}
	}
	if err := s.putContent(ctx, st); err != nil {
		return File{}, err
	}
	expect := "" // not there yet: must still not be there
	if exists {
		expect = cur.SHA256
	}
	if err := s.db.PutFiles(ctx, id, []store.FileWrite{{Path: req.Path, SHA256: st.sha, Size: st.size, If: &expect}},
		s.now(), s.fileQuota(ctx, id)); err != nil {
		return File{}, s.mapStoreErr(req.Path, err)
	}
	s.forgetUsage(id)
	return s.Stat(ctx, owner, id, req.Path)
}

// Stat returns one file or folder.
func (s *Service) Stat(ctx context.Context, owner, id, p string) (File, error) {
	if err := ValidatePath(p); err != nil {
		return File{}, err
	}
	if _, err := s.get(ctx, owner, id); err != nil {
		return File{}, err
	}
	row, err := s.db.GetFile(ctx, id, p)
	if err != nil {
		return File{}, s.mapStoreErr(p, err)
	}
	return fileFromRow(row), nil
}

// CreateFolder makes an empty folder (and any missing parents).
func (s *Service) CreateFolder(ctx context.Context, owner, id, p string) (File, error) {
	if err := ValidatePath(p); err != nil {
		return File{}, err
	}
	if _, err := s.get(ctx, owner, id); err != nil {
		return File{}, err
	}
	if err := s.db.CreateFolder(ctx, id, p, s.now()); err != nil {
		return File{}, s.mapStoreErr(p, err)
	}
	return s.Stat(ctx, owner, id, p)
}

// Move renames or moves a file or folder (with everything in it) in one
// transaction. The target must not exist. With ifMatch, a file is moved only
// if it is still that version.
func (s *Service) Move(ctx context.Context, owner, id, from, to, ifMatch string) (File, error) {
	if err := ValidatePath(from); err != nil {
		return File{}, err
	}
	if err := ValidatePath(to); err != nil {
		return File{}, err
	}
	if from == to {
		return File{}, invalid("the new path is the same as the old one")
	}
	if strings.HasPrefix(to, from+"/") {
		return File{}, invalid("can't move %s into itself", from)
	}
	if _, err := s.get(ctx, owner, id); err != nil {
		return File{}, err
	}
	var ifSHA *string
	if ifMatch != "" {
		ifSHA = &ifMatch
	}
	row, err := s.db.MovePath(ctx, id, from, to, ifSHA, s.now())
	if err != nil {
		return File{}, s.mapStoreErr(to, err)
	}
	return fileFromRow(row), nil
}

// DeletePath removes a file, or a folder: an empty one always, a non-empty
// one only with recursive. The contents stay in object storage until the
// cleanup finds nothing refers to them any more.
func (s *Service) DeletePath(ctx context.Context, owner, id, p string, recursive bool) error {
	if err := ValidatePath(p); err != nil {
		return err
	}
	if _, err := s.get(ctx, owner, id); err != nil {
		return err
	}
	if err := s.db.DeletePath(ctx, id, p, recursive); err != nil {
		return s.mapStoreErr(p, err)
	}
	s.forgetUsage(id)
	return nil
}

// PromoteArtifact copies one of a run's artifacts into the space's files.
// The target must not exist; the quota applies.
func (s *Service) PromoteArtifact(ctx context.Context, owner, id, runID, artifact, to string) (File, error) {
	if err := ValidateArtifactPath(artifact); err != nil {
		return File{}, err
	}
	src, info, err := s.OpenRunFile(ctx, id, runID, RunArtifacts+"/"+artifact)
	if err != nil {
		return File{}, ErrNotFound
	}
	defer func() { _ = src.Close() }()
	if info.Size() > s.cfg.MaxFile {
		return File{}, &LimitError{Msg: fmt.Sprintf("%s is larger than the %d-byte file limit", artifact, s.cfg.MaxFile)}
	}
	f, err := s.WriteFile(ctx, owner, id, WriteRequest{Path: to, Body: src, IfNoneMatch: true})
	var ve *VersionError
	if errors.As(err, &ve) {
		return File{}, ErrExists
	}
	return f, err
}
