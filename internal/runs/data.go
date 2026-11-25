// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
	Types(ctx context.Context, owner string, names map[string]string) (map[string]string, error)
	Fetcher(ctx context.Context, runID, owner string, names map[string]string, expires time.Time) (*isolation.Fetcher, error)
}

// resolveData settles a new run's environment and names. A function's run
// gets only the names its version declared, resolved with the caller's
// bindings, which must match them (name and type) or nothing runs.
func (m *Manager) resolveData(ctx context.Context, owner, spaceID string, req SubmitRequest, sub submission) (string, map[string]string, error) {
	if sub.function != "" && len(sub.connections) == 0 {
		if req.Environment != "" {
			return "", nil, &spaces.InvalidError{Msg: "environment: this function reads no connections"}
		}
		return "", nil, nil
	}
	if m.d.Data == nil {
		if sub.function != "" {
			return "", nil, &spaces.InvalidError{Msg: "this function reads connections, and connections are off on this nbsandbox"}
		}
		if req.Environment != "" {
			return "", nil, &spaces.InvalidError{Msg: "environment: connections are off on this nbsandbox"}
		}
		return "", nil, nil
	}
	env, names, err := m.d.Data.Resolve(ctx, spaceID, req.Environment)
	if err != nil || sub.function == "" {
		return env, names, err
	}
	types, err := m.d.Data.Types(ctx, owner, names)
	if err != nil {
		return "", nil, err
	}
	var missing []string
	own := map[string]string{}
	for name, typ := range sub.connections {
		switch {
		case names[name] == "":
			missing = append(missing, fmt.Sprintf("%s (%s, not bound)", name, typ))
		case types[name] != typ:
			missing = append(missing, fmt.Sprintf("%s (%s, bound to a %s connection)", name, typ, types[name]))
		default:
			own[name] = names[name]
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		where := "this space"
		if env != "" {
			where = "this space's environment " + env
		}
		return "", nil, &spaces.InvalidError{Msg: fmt.Sprintf("function %s needs connections %s doesn't give it: %s",
			sub.function, where, strings.Join(missing, ", "))}
	}
	return env, own, nil
}

// declaredConnections is what a version being published reads: the names
// its space binds (in the default environment) and their types.
func (m *Manager) declaredConnections(ctx context.Context, owner, spaceID string) (string, error) {
	if m.d.Data == nil {
		return "{}", nil
	}
	_, names, err := m.d.Data.Resolve(ctx, spaceID, "")
	if err != nil {
		return "", err
	}
	types, err := m.d.Data.Types(ctx, owner, names)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(types)
	return string(b), nil
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
