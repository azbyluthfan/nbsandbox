// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// StatusEvent is the body of one status event, as streams send it.
type StatusEvent struct {
	RunID         string        `json:"run_id"`
	Status        string        `json:"status"`
	Function      string        `json:"function,omitempty"`
	Version       int           `json:"version,omitempty"`
	QueuePosition int           `json:"queue_position,omitempty"` // when queued: the position at that moment
	Result        *CallerResult `json:"result,omitempty"`         // when final
}

func (c *core) event(r store.Run, status string, pos int, res *CallerResult) *store.Event {
	b, _ := json.Marshal(StatusEvent{RunID: r.ID, Status: status, Function: r.Function, Version: r.Version,
		QueuePosition: pos, Result: res})
	return &store.Event{SpaceID: r.SpaceID, RunID: r.ID, Function: r.Function, Status: status, Data: string(b),
		CreatedAt: c.d.Now()}
}

// EventsQuery selects status events.
type EventsQuery struct {
	RunID    string // "" = every run in the space
	Function string // "" = any
	After    int64  // sequence number already seen
	// Include adds what runs report while they work: "progress" and/or
	// "cell". Status changes are always sent; a streamed output's items are
	// on the result stream (FollowResult), not here.
	Include map[string]bool
}

// Events returns status events after q.After, and a channel that is closed
// when the space records its next event. ErrEventsPruned means some events
// after q.After were already deleted (older than runs.event_retention).
func (m *Manager) Events(ctx context.Context, owner, spaceID string, q EventsQuery) ([]store.Event, <-chan struct{}, error) {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return nil, nil, err
	}
	// Take the wake-up channel before reading, so an event recorded in
	// between still wakes the caller.
	wake := m.wake.Wait(spaceID)
	evs, err := m.d.DB.Events(ctx, store.EventFilter{SpaceID: spaceID, RunID: q.RunID, Function: q.Function, After: q.After})
	if err != nil {
		return nil, nil, err
	}
	if q.After > 0 {
		if oldest, err := m.d.DB.OldestEventSeq(ctx); err == nil && oldest > q.After+1 {
			return evs, wake, ErrEventsPruned
		}
	}
	return evs, wake, nil
}

// ErrEventsPruned: the client asked to resume from events that were already
// deleted; it should re-read the runs it cares about.
var ErrEventsPruned = errors.New("some events were older than the event retention and were deleted")

// Wake returns a channel closed at the space's next status event (the
// caller has already checked ownership).
func (m *Manager) Wake(spaceID string) <-chan struct{} { return m.wake.Wait(spaceID) }

// RunExists checks a run belongs to the space (for per-run streams).
func (m *Manager) RunExists(ctx context.Context, owner, spaceID, runID string) error {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return err
	}
	if _, err := m.d.DB.GetRun(ctx, spaceID, runID); err != nil {
		return spaces.ErrNotFound
	}
	return nil
}
