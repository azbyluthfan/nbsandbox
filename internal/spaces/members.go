// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Member is a role in a space (docs/plan-shared-spaces.md). The space's
// creator is listed first, an owner with Creator set.
type Member struct {
	Principal string
	Role      Role
	Creator   bool
	GrantedBy string    // "" for the creator
	GrantedAt time.Time // zero for the creator
}

// MaxPrincipal is the longest caller ID a space grants a role to.
const MaxPrincipal = 256

// validPrincipal checks a caller ID: what nbsandbox records as a caller (a
// username, or <prefix>:<claims> for a token), not checked against anyone.
func validPrincipal(p string) error {
	if p == "" || len(p) > MaxPrincipal || strings.TrimSpace(p) != p {
		return invalid("a principal is a caller ID: 1 to 256 characters, without leading or trailing spaces")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return invalid("a principal can't contain control characters")
		}
	}
	return nil
}

// Members lists a space's roles (any role may).
func (s *Service) Members(ctx context.Context, caller, id string) ([]Member, error) {
	sp, err := s.access(ctx, caller, id, RoleRead)
	if err != nil {
		return nil, err
	}
	ms, err := s.db.SpaceMembers(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []Member{{Principal: sp.Owner, Role: RoleOwner, Creator: true}}
	for _, m := range ms {
		out = append(out, Member{Principal: m.Principal, Role: Role(m.Role), GrantedBy: m.GrantedBy, GrantedAt: m.GrantedAt})
	}
	return out, nil
}

// PutMember grants principal a role in the space, or changes it (owners).
// The creator's own role can't change: hand the space over first.
func (s *Service) PutMember(ctx context.Context, caller, id, principal string, role Role) (Member, error) {
	if err := validPrincipal(principal); err != nil {
		return Member{}, err
	}
	if !ValidRole(role) {
		return Member{}, invalid("role must be read, write or owner")
	}
	sp, err := s.access(ctx, caller, id, RoleOwner)
	if err != nil {
		return Member{}, err
	}
	if principal == sp.Owner {
		return Member{}, invalid("%s created this space and is always an owner; hand the space over to change that", principal)
	}
	m := store.Member{Principal: principal, Role: string(role), GrantedBy: caller, GrantedAt: s.now()}
	if err := s.db.PutSpaceMember(ctx, id, m); err != nil {
		return Member{}, err
	}
	return Member{Principal: principal, Role: role, GrantedBy: caller, GrantedAt: m.GrantedAt}, nil
}

// RemoveMember revokes principal's role: owners may revoke anyone's, anyone
// their own (leaving). The creator can't be removed.
func (s *Service) RemoveMember(ctx context.Context, caller, id, principal string) error {
	if err := validPrincipal(principal); err != nil {
		return err
	}
	need := RoleOwner
	if principal == caller {
		need = RoleRead
	}
	sp, err := s.access(ctx, caller, id, need)
	if err != nil {
		return err
	}
	if principal == sp.Owner {
		return invalid("%s created this space and can't be removed; they can hand it over to another owner first", principal)
	}
	if err := s.db.DeleteSpaceMember(ctx, id, principal); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Transfer hands the space over: to, already one of its owners, becomes its
// creator and account; the caller, its creator until now, stays an owner.
func (s *Service) Transfer(ctx context.Context, caller, id, to string) (Space, error) {
	if err := validPrincipal(to); err != nil {
		return Space{}, err
	}
	sp, err := s.access(ctx, caller, id, RoleOwner)
	if err != nil {
		return Space{}, err
	}
	if sp.Owner != caller {
		return Space{}, &ForbiddenError{Need: RoleOwner, Msg: "only the space's creator (" + sp.Owner + ") can hand it over"}
	}
	if to == caller {
		return Space{}, invalid("you already are this space's creator")
	}
	r, err := s.db.SpaceAccess(ctx, to, id)
	if err != nil || r.Role != store.RoleOwner {
		return Space{}, invalid("%s must be an owner of the space first", to)
	}
	if err := s.db.TransferSpace(ctx, id, caller, to, s.now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Space{}, ErrNotFound
		}
		return Space{}, err
	}
	return s.Get(ctx, caller, id)
}
