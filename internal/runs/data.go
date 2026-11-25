// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// DataAccess gives runs their external data (docs/plan-data-access.md):
// it settles which connection each of a space's names means when a run is
// submitted, and makes the run's fetcher when it starts
// (internal/connections).
type DataAccess interface {
	Resolve(ctx context.Context, spaceID, environment string) (string, map[string]string, error)
	Fetcher(ctx context.Context, runID, owner string, names map[string]string, expires time.Time) (*isolation.Fetcher, error)
}

// resolveData settles a new run's environment and names. Published
// functions read nothing outside yet: they will read with their caller's
// connections.
func (m *Manager) resolveData(ctx context.Context, spaceID string, req SubmitRequest, sub submission) (string, map[string]string, error) {
	if m.d.Data == nil || sub.function != "" {
		if req.Environment != "" {
			return "", nil, &spaces.InvalidError{Msg: "environment: this run can't read connections"}
		}
		return "", nil, nil
	}
	return m.d.Data.Resolve(ctx, spaceID, req.Environment)
}

// fetcherFor makes a starting run's fetcher, if its space binds names.
func (e *executor) fetcherFor(ctx context.Context, r store.Run) (*isolation.Fetcher, error) {
	if r.Environment == "" || e.d.Data == nil {
		return nil, nil
	}
	names, err := e.d.DB.RunConnections(ctx, r.ID)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	// The gateway token outlives the run's timeout a little: the run is
	// stopped by then whatever happens.
	return e.d.Data.Fetcher(ctx, r.ID, r.Owner, names, e.d.Now().Add(e.cfg.Timeout+5*time.Minute))
}
