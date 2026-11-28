// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"fmt"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// AdminView is a run for administrators (its owner is View's): metadata
// only, never results or outputs.
type AdminView struct {
	View
}

// AdminFilter narrows AdminList; empty fields match everything.
type AdminFilter struct {
	Status  string
	Owner   string
	SpaceID string
	Worker  string
}

func (m *Manager) adminView(ctx context.Context, r store.Run) AdminView {
	v := m.view(ctx, r, false)
	v.Result = nil
	return AdminView{View: v}
}

// AdminList returns one page of every owner's runs, newest first.
func (m *Manager) AdminList(ctx context.Context, f AdminFilter, cursor string, limit int) ([]AdminView, string, error) {
	if f.Status != "" && !KnownStatus(f.Status) {
		return nil, "", &spaces.InvalidError{Msg: "unknown status " + f.Status}
	}
	if f.SpaceID != "" && !spaces.ValidID(f.SpaceID) {
		return nil, "", &spaces.InvalidError{Msg: "space must be a lowercase UUID"}
	}
	page, err := m.d.DB.ListRuns(ctx, f.SpaceID, store.RunFilter{Status: f.Status, Owner: f.Owner, Worker: f.Worker}, cursor, limit)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, "", &spaces.InvalidError{Msg: "invalid cursor"}
	}
	if err != nil {
		return nil, "", err
	}
	out := make([]AdminView, 0, len(page.Items))
	for _, r := range page.Items {
		out = append(out, m.adminView(ctx, r))
	}
	return out, page.NextCursor, nil
}

// AdminCancel cancels any owner's run; by is the administrator.
func (m *Manager) AdminCancel(ctx context.Context, runID, by, reason string) (AdminView, error) {
	if len(reason) > MaxCancelReason {
		return AdminView{}, &spaces.InvalidError{Msg: fmt.Sprintf("reason is longer than %d bytes", MaxCancelReason)}
	}
	r, err := m.d.DB.RunByID(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return AdminView{}, spaces.ErrNotFound
	}
	if err != nil {
		return AdminView{}, err
	}
	if _, err := m.cancel(ctx, r, by, reason); err != nil {
		return AdminView{}, err
	}
	r, err = m.d.DB.RunByID(ctx, runID)
	if err != nil {
		return AdminView{}, err
	}
	return m.adminView(ctx, r), nil
}

// MaxDrainReason bounds the reason given for draining.
const MaxDrainReason = 500

// DrainState is whether the workers are drained.
func (m *Manager) DrainState(ctx context.Context) (store.Drain, error) { return m.d.DB.GetDrain(ctx) }

// SetDrain drains the workers (no queued run starts; running ones finish)
// or resumes them. by is the administrator.
func (m *Manager) SetDrain(ctx context.Context, draining bool, reason, by string) (store.Drain, error) {
	if len(reason) > MaxDrainReason {
		return store.Drain{}, &spaces.InvalidError{Msg: fmt.Sprintf("reason is longer than %d bytes", MaxDrainReason)}
	}
	if err := m.d.DB.SetDrain(ctx, draining, reason, by, m.d.Now()); err != nil {
		return store.Drain{}, err
	}
	if draining {
		m.d.Log.Warn("workers drained: no queued run starts until resumed", "by", by, "reason", reason)
	} else {
		m.d.Log.Info("workers resumed", "by", by)
		// Every worker looks for queued runs again.
		if err := m.d.DB.Notify(ctx, store.ChanQueued, ""); err != nil {
			m.d.Log.Warn("notify the workers of the resume", "error", err) // they poll too
		}
		m.ex.signal()
	}
	return m.d.DB.GetDrain(ctx)
}
