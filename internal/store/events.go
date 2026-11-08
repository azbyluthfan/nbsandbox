// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"time"
)

// Event is a run status change, recorded for the SSE status streams.
type Event struct {
	Seq                      int64
	SpaceID, RunID, Function string
	Status                   string // queued | running | <terminal status>
	Kind                     string // status (default) | item | progress | cell
	Data                     string // the event body (JSON)
	CreatedAt                time.Time
}

// eventLock serialises event inserts until their transaction commits, so
// sequence numbers become visible in order. Without it, a stream could read
// event 42 while 41's transaction hasn't committed, resume after 42, and
// never see 41. Events are few (a handful per run), so the wait is short.
const eventLock = 7_310_002

// insertEvent records ev inside tx and returns its sequence number.
// Every node listening hears of it when tx commits (ChanEvents, with the
// space ID).
func (db *DB) insertEvent(ctx context.Context, tx txn, ev Event) (int64, error) {
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", eventLock); err != nil {
		return 0, err
	}
	var seq int64
	err := tx.QueryRowContext(ctx,
		"INSERT INTO run_events(space_id, run_id, function, status, kind, data, created_at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING seq",
		ev.SpaceID, ev.RunID, ev.Function, ev.Status, ev.kind(), ev.Data, ev.CreatedAt.UnixMilli()).Scan(&seq)
	if err != nil {
		return 0, err
	}
	return seq, db.notifyTx(ctx, tx, ChanEvents, ev.SpaceID)
}

// AddEvent records an event on its own (e.g. a queue position change).
func (db *DB) AddEvent(ctx context.Context, ev Event) (int64, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	seq, err := db.insertEvent(ctx, tx, ev)
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

func (ev Event) kind() string {
	if ev.Kind == "" {
		return EventStatus
	}
	return ev.Kind
}

// Event kinds.
const (
	EventStatus   = "status"
	EventItem     = "item"
	EventProgress = "progress"
	EventCell     = "cell"
)

// EventFilter selects events after a sequence number.
type EventFilter struct {
	SpaceID  string
	RunID    string // "" = every run in the space
	Function string // "" = any
	Kind     string // "" = any
	After    int64
	Limit    int
}

// Events returns events in sequence order.
func (db *DB) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	q := "SELECT seq, space_id, run_id, function, status, kind, data, created_at FROM run_events WHERE space_id = ? AND seq > ?"
	args := []any{f.SpaceID, f.After}
	if f.RunID != "" {
		q += " AND run_id = ?"
		args = append(args, f.RunID)
	}
	if f.Function != "" {
		q += " AND function = ?"
		args = append(args, f.Function)
	}
	if f.Kind != "" {
		q += " AND kind = ?"
		args = append(args, f.Kind)
	}
	if f.Limit <= 0 {
		f.Limit = 1000
	}
	q += " ORDER BY seq LIMIT ?"
	args = append(args, f.Limit)
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var ev Event
		var at int64
		if err := rows.Scan(&ev.Seq, &ev.SpaceID, &ev.RunID, &ev.Function, &ev.Status, &ev.Kind, &ev.Data, &at); err != nil {
			return nil, err
		}
		ev.CreatedAt = time.UnixMilli(at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// OldestEventSeq returns the lowest sequence number still kept (0 if none).
// Sequence numbers are shared by all spaces, so a client resuming after a
// number below this one may have missed pruned events.
func (db *DB) OldestEventSeq(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	err := db.r.QueryRowContext(ctx, "SELECT MIN(seq) FROM run_events").Scan(&seq)
	return seq.Int64, err
}

// DeleteEventsBefore prunes old events.
func (db *DB) DeleteEventsBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := db.w.ExecContext(ctx, "DELETE FROM run_events WHERE created_at < ?", t.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
