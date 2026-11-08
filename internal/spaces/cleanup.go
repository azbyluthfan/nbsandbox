// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"path"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
)

// CleanupGrace is how old unreferenced contents must be before cleanup
// removes them. Contents being stored are written (or refreshed, see
// blob.RefreshAfter) just before the index refers to them, so anything this
// old and unreferenced is no longer wanted.
const CleanupGrace = time.Hour

// CollectGarbage removes contents from object storage that no file in any
// space refers to and that were last written more than grace ago (at least
// CleanupGrace). Each candidate is checked again just before it goes. It
// returns how many objects it removed.
func (s *Service) CollectGarbage(ctx context.Context, grace time.Duration) (int, error) {
	grace = max(grace, CleanupGrace)
	refs, err := s.referenced(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := s.now().Add(-grace)
	var candidates []string
	if err := s.blobs.List(ctx, "sha256/", func(i blob.Info) error {
		if !refs[path.Base(i.Key)] && i.Modified.Before(cutoff) {
			candidates = append(candidates, i.Key)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	// References added while listing count too.
	if refs, err = s.referenced(ctx); err != nil {
		return 0, err
	}
	removed := 0
	for _, key := range candidates {
		if refs[path.Base(key)] {
			continue
		}
		info, err := s.blobs.Head(ctx, key)
		if err != nil || !info.Modified.Before(cutoff) {
			continue // gone already, or written again just now
		}
		if err := s.blobs.Delete(ctx, key); err != nil {
			s.log.Warn("cleanup: remove unreferenced contents", "key", key, "error", err)
			continue
		}
		removed++
	}
	return removed, nil
}

func (s *Service) referenced(ctx context.Context) (map[string]bool, error) {
	refs := map[string]bool{}
	err := s.db.ReferencedContent(ctx, func(sha string) error {
		refs[sha] = true
		return nil
	})
	return refs, err
}
