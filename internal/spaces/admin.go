// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"errors"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Administration: every owner's spaces, for callers with the admin scope
// (the API checks it). nbsandbox keeps no user accounts, so this is how a
// space outlives its owner: a user removed from the identity provider, or
// an issuer whose IDs changed.

// AdminList returns one page of every owner's spaces, or one owner's when
// owner isn't "", newest first, with their owners.
func (s *Service) AdminList(ctx context.Context, owner, cursor string, limit int) ([]Space, string, error) {
	page, err := s.db.ListAllSpaces(ctx, owner, cursor, limit)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, "", invalid("invalid cursor")
	}
	if err != nil {
		return nil, "", err
	}
	out := make([]Space, 0, len(page.Items))
	for _, sp := range page.Items {
		v := fromStore(sp)
		v.Owner = sp.Owner
		out = append(out, v)
	}
	return out, page.NextCursor, nil
}

// AdminGet returns any space with its owner and usage.
func (s *Service) AdminGet(ctx context.Context, id string) (Space, error) {
	owner, err := s.ownerOf(ctx, id)
	if err != nil {
		return Space{}, err
	}
	sp, err := s.Get(ctx, owner, id)
	sp.Owner = owner
	return sp, err
}

// AdminSetOwner gives a space to another owner ID.
func (s *Service) AdminSetOwner(ctx context.Context, id, newOwner string) (Space, error) {
	if newOwner == "" || len(newOwner) > 512 {
		return Space{}, invalid("owner must be a caller ID (1 to 512 characters)")
	}
	if _, err := s.ownerOf(ctx, id); err != nil {
		return Space{}, err
	}
	if err := s.db.SetSpaceOwner(ctx, id, newOwner, s.now()); errors.Is(err, store.ErrNotFound) {
		return Space{}, ErrNotFound
	} else if err != nil {
		return Space{}, err
	}
	s.forgetUsage(id) // the quota may differ
	return s.AdminGet(ctx, id)
}

// AdminDelete deletes any space (refused while it has an active run).
func (s *Service) AdminDelete(ctx context.Context, id string) error {
	owner, err := s.ownerOf(ctx, id)
	if err != nil {
		return err
	}
	return s.Delete(ctx, owner, id)
}

func (s *Service) ownerOf(ctx context.Context, id string) (string, error) {
	if !ValidID(id) {
		return "", ErrNotFound
	}
	owner, err := s.db.SpaceOwner(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotFound
	}
	return owner, err
}
