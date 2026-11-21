// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session modes and statuses.
const (
	SessionReadWrite = "read-write"
	SessionReadOnly  = "read-only"

	SessionStarting = "starting"
	SessionReady    = "ready"
	SessionEnded    = "ended"
)

// ChanSessions is notified when a session waits for a worker.
const ChanSessions = "sessions"

var (
	// ErrSessionLimit means the owner already has as many open sessions as
	// allowed.
	ErrSessionLimit = errors.New("too many open sessions")
	// ErrWritingSession means a read-write session holds the space's write
	// lock. It is an ErrActiveRun, so callers that refuse a second writer
	// refuse it the same way.
	ErrWritingSession = fmt.Errorf("%w: a read-write session is open in the space", ErrActiveRun)
)

// Session is an interactive session's record.
type Session struct {
	ID, SpaceID, Owner, Notebook string
	Mode                         string // SessionReadWrite or SessionReadOnly
	Status                       string // SessionStarting, SessionReady or SessionEnded
	Worker                       string // "" until a worker takes it
	CreatedAt                    time.Time
	EndedAt                      time.Time // zero while open
	EndReason, EndedBy           string
}

// Open reports whether the session hasn't ended.
func (s Session) Open() bool { return s.Status != SessionEnded }

const sessionCols = "id, space_id, owner, notebook, mode, status, worker, created_at, ended_at, end_reason, ended_by"

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var created int64
	var ended sql.NullInt64
	err := sc.Scan(&s.ID, &s.SpaceID, &s.Owner, &s.Notebook, &s.Mode, &s.Status, &s.Worker, &created, &ended, &s.EndReason, &s.EndedBy)
	if err != nil {
		return Session{}, err
	}
	s.CreatedAt = time.UnixMilli(created)
	if ended.Valid {
		s.EndedAt = time.UnixMilli(ended.Int64)
	}
	return s, nil
}

// OpenSession returns the notebook's open session (created false), or
// records a new one waiting for a worker (status starting). A read-write
// session is given only while nothing else can write the space (no queued or
// running writing run, no other read-write session); otherwise the new
// session is read-only. ErrSessionLimit if the owner already has perUser
// open sessions (0: no limit); ErrNotFound if the space doesn't exist.
func (db *DB) OpenSession(ctx context.Context, s Session, perUser int) (Session, bool, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSpace(ctx, tx, s.SpaceID); err != nil {
		return Session{}, false, err
	}
	existing, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionCols+
		" FROM sessions WHERE space_id = ? AND notebook = ? AND status <> 'ended'", s.SpaceID, s.Notebook))
	switch {
	case err == nil:
		return existing, false, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return Session{}, false, err
	}
	if perUser > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE owner = ? AND status <> 'ended'", s.Owner).Scan(&n); err != nil {
			return Session{}, false, err
		}
		if n >= perUser {
			return Session{}, false, ErrSessionLimit
		}
	}
	if s.Mode == SessionReadWrite {
		busy, err := spaceHasWriter(ctx, tx, s.SpaceID)
		if err != nil {
			return Session{}, false, err
		}
		if busy {
			s.Mode = SessionReadOnly
		}
	}
	s.Status, s.Worker, s.EndedAt, s.EndReason, s.EndedBy = SessionStarting, "", time.Time{}, "", ""
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, space_id, owner, notebook, mode, status, created_at)
		VALUES (?, ?, ?, ?, ?, 'starting', ?)`, s.ID, s.SpaceID, s.Owner, s.Notebook, s.Mode, s.CreatedAt.UnixMilli()); err != nil {
		return Session{}, false, err
	}
	if err := db.notifyTx(ctx, tx, ChanSessions, s.ID); err != nil {
		return Session{}, false, err
	}
	return s, true, tx.Commit()
}

// spaceHasWriter reports whether a queued or running writing run, or an open
// read-write session, holds the space's write lock. The caller holds the
// space's row lock (lockSpace).
func spaceHasWriter(ctx context.Context, tx txn, spaceID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM runs WHERE space_id = ? AND status IN ('queued', 'running') AND writes_space = 1) +
		(SELECT count(*) FROM sessions WHERE space_id = ? AND status <> 'ended' AND mode = 'read-write')`,
		spaceID, spaceID).Scan(&n)
	return n > 0, err
}

// writingSessionOpen reports whether the space has an open read-write
// session. The caller holds the space's row lock.
func writingSessionOpen(ctx context.Context, tx txn, spaceID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE space_id = ? AND status <> 'ended' AND mode = 'read-write'",
		spaceID).Scan(&n)
	return n > 0, err
}

// ClaimSession gives worker the oldest session waiting for one, if worker
// has fewer than perWorker open sessions (0: no limit) and the workers
// aren't drained. It reports false when there is none to take.
func (db *DB) ClaimSession(ctx context.Context, worker string, perWorker int) (Session, bool, error) {
	s, err := scanSession(db.w.QueryRowContext(ctx, `
		UPDATE sessions SET worker = ? WHERE id = (
			SELECT id FROM sessions WHERE status = 'starting' AND worker = ''
			ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED)
		AND (? = 0 OR (SELECT count(*) FROM sessions WHERE worker = ? AND status <> 'ended') < ?)
		AND NOT (SELECT draining FROM drain WHERE id = 1)
		RETURNING `+sessionCols, worker, perWorker, worker, perWorker))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	return s, err == nil, err
}

// AdoptSession gives an open session to worker (its container is on that
// worker's Docker: the worker restarted under another ID, or the one that
// held it is gone). It returns the session, or ErrNotFound if it has ended.
func (db *DB) AdoptSession(ctx context.Context, id, worker string) (Session, error) {
	s, err := scanSession(db.w.QueryRowContext(ctx, "UPDATE sessions SET worker = ? WHERE id = ? AND status <> 'ended' RETURNING "+sessionCols,
		worker, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

// MarkSessionReady moves a starting session to ready. It reports false if it
// was no longer starting (ended meanwhile).
func (db *DB) MarkSessionReady(ctx context.Context, id string) (bool, error) {
	res, err := db.w.ExecContext(ctx, "UPDATE sessions SET status = 'ready' WHERE id = ? AND status = 'starting'", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// EndSession ends an open session. It reports whether this call ended it
// (false: it had already ended, or doesn't exist).
func (db *DB) EndSession(ctx context.Context, id, reason, by string, at time.Time) (bool, error) {
	res, err := db.w.ExecContext(ctx, `UPDATE sessions SET status = 'ended', ended_at = ?, end_reason = ?, ended_by = ?
		WHERE id = ? AND status <> 'ended'`, at.UnixMilli(), reason, by, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// GetSession returns a session, open or ended; ErrNotFound if none.
func (db *DB) GetSession(ctx context.Context, id string) (Session, error) {
	s, err := scanSession(db.r.QueryRowContext(ctx, "SELECT "+sessionCols+" FROM sessions WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

// SessionFilter narrows OpenSessions; empty fields match everything.
type SessionFilter struct {
	SpaceID, Owner, Worker string
}

// OpenSessions lists open sessions, oldest first.
func (db *DB) OpenSessions(ctx context.Context, f SessionFilter) ([]Session, error) {
	rows, err := db.r.QueryContext(ctx, "SELECT "+sessionCols+` FROM sessions
		WHERE status <> 'ended' AND (? = '' OR space_id = ?) AND (? = '' OR owner = ?) AND (? = '' OR worker = ?)
		ORDER BY created_at, id`, f.SpaceID, f.SpaceID, f.Owner, f.Owner, f.Worker, f.Worker)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteEndedSessionsBefore removes the records of sessions that ended
// before t.
func (db *DB) DeleteEndedSessionsBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := db.w.ExecContext(ctx, "DELETE FROM sessions WHERE status = 'ended' AND ended_at < ?", t.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
