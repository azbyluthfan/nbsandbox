// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrIdempotencyMismatch: the Idempotency-Key was used with a different body.
var ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")

// Run is a row in runs.
type Run struct {
	ID, SpaceID, Owner, Notebook, ParametersJSON string
	Status, ErrorKind, Fault, Message            string
	DependencyWarnings                           string // JSON array or ""
	CreatedAt                                    time.Time
	StartedAt, FinishedAt                        *time.Time
	QueueSeq                                     int64
	Function                                     string // published function, "" for a plain run
	Version                                      int    // its version, 0 for a plain run
	NotebookSHA256                               string // of the snapshot the run executes
	OutputSchema                                 string // the contract's output schema, "" if none (of each item when streamed)
	OutputMode                                   string // single | stream
	ClientApp                                    string // the application that called on its user's behalf
	WritesSpace                                  bool   // false: /space is mounted read-only
	CancelledBy, CancelReason                    string
	OutputBytes                                  int64
	Worker                                       string // the worker holding the run's lease, "" if none
	Token                                        int64  // the current lease's token (0 before the first claim)
	LeaseUntil                                   time.Time
}

// Terminal reports whether the run has finished.
func (r Run) Terminal() bool { return r.Status != "queued" && r.Status != "running" }

const runCols = "id, space_id, owner, notebook, parameters_json, status, error_kind, fault, message, dependency_warnings, " +
	"created_at, started_at, finished_at, queue_seq, function, version, notebook_sha256, output_schema, output_mode, client_app, writes_space, " +
	"cancelled_by, cancel_reason, output_bytes, worker, token, lease_until"

func scanRun(s interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var created int64
	var started, finished sql.NullInt64
	var writes int
	var lease int64
	err := s.Scan(&r.ID, &r.SpaceID, &r.Owner, &r.Notebook, &r.ParametersJSON, &r.Status, &r.ErrorKind,
		&r.Fault, &r.Message, &r.DependencyWarnings, &created, &started, &finished, &r.QueueSeq,
		&r.Function, &r.Version, &r.NotebookSHA256, &r.OutputSchema, &r.OutputMode, &r.ClientApp, &writes, &r.CancelledBy, &r.CancelReason, &r.OutputBytes,
		&r.Worker, &r.Token, &lease)
	r.WritesSpace = writes != 0
	if err != nil {
		return Run{}, err
	}
	r.CreatedAt = time.UnixMilli(created)
	if lease > 0 {
		r.LeaseUntil = time.UnixMilli(lease)
	}
	if started.Valid {
		t := time.UnixMilli(started.Int64)
		r.StartedAt = &t
	}
	if finished.Valid {
		t := time.UnixMilli(finished.Int64)
		r.FinishedAt = &t
	}
	return r, nil
}

// GetRun returns a run in a space.
func (db *DB) GetRun(ctx context.Context, spaceID, runID string) (Run, error) {
	r, err := scanRun(db.r.QueryRowContext(ctx, "SELECT "+runCols+" FROM runs WHERE id = ? AND space_id = ?", runID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// RunByID returns a run regardless of space (internal use only).
func (db *DB) RunByID(ctx context.Context, runID string) (Run, error) {
	r, err := scanRun(db.r.QueryRowContext(ctx, "SELECT "+runCols+" FROM runs WHERE id = ?", runID))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// RunFilter narrows ListRuns.
type RunFilter struct {
	Status        string
	Function      string // only invocations of this function
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
}

// RunPage is one page of runs.
type RunPage struct {
	Items      []Run
	NextCursor string
}

// ListRuns lists a space's runs, newest first, with cursor paging.
func (db *DB) ListRuns(ctx context.Context, spaceID string, f RunFilter, cursor string, limit int) (RunPage, error) {
	q := "SELECT " + runCols + " FROM runs WHERE space_id = ?"
	args := []any{spaceID}
	if f.Status != "" {
		q += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.Function != "" {
		q += " AND function = ?"
		args = append(args, f.Function)
	}
	if f.CreatedAfter != nil {
		q += " AND created_at > ?"
		args = append(args, f.CreatedAfter.UnixMilli())
	}
	if f.CreatedBefore != nil {
		q += " AND created_at < ?"
		args = append(args, f.CreatedBefore.UnixMilli())
	}
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return RunPage{}, err
		}
		q += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, at, at, id)
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit+1)
	runs, err := db.queryRuns(ctx, q, args...)
	if err != nil {
		return RunPage{}, err
	}
	page := RunPage{Items: runs}
	if len(runs) > limit {
		last := runs[limit-1]
		page.Items = runs[:limit]
		page.NextCursor = encodeCursor(last.CreatedAt.UnixMilli(), last.ID)
	}
	return page, nil
}

func (db *DB) queryRuns(ctx context.Context, q string, args ...any) ([]Run, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunsWithStatus returns runs in a status, oldest queue position first.
func (db *DB) RunsWithStatus(ctx context.Context, status string) ([]Run, error) {
	return db.queryRuns(ctx, "SELECT "+runCols+" FROM runs WHERE status = ? ORDER BY queue_seq", status)
}

// FinishedBefore returns finished runs that ended before t (retention).
func (db *DB) FinishedBefore(ctx context.Context, t time.Time) ([]Run, error) {
	return db.queryRuns(ctx, "SELECT "+runCols+
		" FROM runs WHERE status NOT IN ('queued', 'running') AND finished_at < ? ORDER BY finished_at", t.UnixMilli())
}

// DeleteRun removes a finished run's row; ErrActiveRun if it hasn't finished.
func (db *DB) DeleteRun(ctx context.Context, spaceID, runID string) error {
	res, err := db.w.ExecContext(ctx,
		"DELETE FROM runs WHERE id = ? AND space_id = ? AND status NOT IN ('queued', 'running')", runID, spaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	if _, err := db.GetRun(ctx, spaceID, runID); err != nil {
		return err
	}
	return ErrActiveRun
}

// Idempotency identifies a submission for replay protection.
type Idempotency struct {
	Owner, Key, BodyHash string
	Since                time.Time // keys older than this are expired
}

// SubmitRun inserts a queued run, honouring an optional idempotency key, in
// one transaction. It returns the ID of the run that represents this
// submission and whether it was a replay of an earlier one.
func (db *DB) SubmitRun(ctx context.Context, r NewRun, idem *Idempotency) (string, bool, error) {
	id, replay, _, err := db.SubmitRunWithEvent(ctx, r, idem, nil)
	return id, replay, err
}

// SubmitRunWithEvent is SubmitRun that also records ev (the "queued" status
// event) in the same transaction, unless the submission is a replay. It
// returns the event's sequence number (0 if none was recorded).
func (db *DB) SubmitRunWithEvent(ctx context.Context, r NewRun, idem *Idempotency, ev *Event) (string, bool, int64, error) {
	id, replay, seq, err := db.submitRun(ctx, r, idem, ev)
	if idem != nil && uniqueViolation(err, "idempotency_pkey") {
		// The same key arrived twice at once and the other request won: now
		// its row is there, and this one replays it (or is refused as a
		// mismatch).
		return db.submitRun(ctx, r, idem, ev)
	}
	return id, replay, seq, err
}

func (db *DB) submitRun(ctx context.Context, r NewRun, idem *Idempotency, ev *Event) (string, bool, int64, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return "", false, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if idem != nil {
		var hash, runID string
		var created int64
		err := tx.QueryRowContext(ctx, "SELECT body_hash, run_id, created_at FROM idempotency WHERE owner = ? AND key = ?",
			idem.Owner, idem.Key).Scan(&hash, &runID, &created)
		switch {
		case err == nil && time.UnixMilli(created).After(idem.Since):
			if hash != idem.BodyHash {
				return "", false, 0, ErrIdempotencyMismatch
			}
			return runID, true, 0, nil
		case err == nil: // expired: forget it
			if _, err := tx.ExecContext(ctx, "DELETE FROM idempotency WHERE owner = ? AND key = ?", idem.Owner, idem.Key); err != nil {
				return "", false, 0, err
			}
		case !errors.Is(err, sql.ErrNoRows):
			return "", false, 0, err
		}
	}
	if r.ParametersJSON == "" {
		r.ParametersJSON = "{}"
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs(id, space_id, owner, notebook, parameters_json, dependency_warnings, status, created_at,
		                 function, version, notebook_sha256, output_schema, output_mode, client_app, writes_space)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.SpaceID, r.Owner, r.Notebook, r.ParametersJSON, r.DependencyWarnings, r.CreatedAt.UnixMilli(),
		r.Function, r.Version, r.NotebookSHA256, r.OutputSchema, modeOrSingle(r.OutputMode), r.ClientApp, boolInt(!r.ReadOnlySpace))
	if err != nil {
		if uniqueViolation(err, "one_active_writer_per_space") {
			return "", false, 0, ErrActiveRun
		}
		return "", false, 0, err
	}
	for _, f := range r.Files {
		if _, err := tx.ExecContext(ctx, "INSERT INTO run_files(run_id, path, sha256, size) VALUES (?, ?, ?, ?)",
			r.ID, f.Path, f.SHA256, f.Size); err != nil {
			return "", false, 0, err
		}
	}
	if idem != nil {
		if _, err := tx.ExecContext(ctx, "INSERT INTO idempotency(owner, key, body_hash, run_id, created_at) VALUES (?, ?, ?, ?, ?)",
			idem.Owner, idem.Key, idem.BodyHash, r.ID, r.CreatedAt.UnixMilli()); err != nil {
			return "", false, 0, err // a unique violation here: see SubmitRunWithEvent
		}
	}
	var seq int64
	if ev != nil {
		if seq, err = db.insertEvent(ctx, tx, *ev); err != nil {
			return "", false, 0, err
		}
	}
	if err := db.notifyTx(ctx, tx, ChanQueued, r.ID); err != nil {
		return "", false, 0, err
	}
	return r.ID, false, seq, tx.Commit()
}

// DeleteIdempotencyBefore removes expired idempotency keys.
func (db *DB) DeleteIdempotencyBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := db.w.ExecContext(ctx, "DELETE FROM idempotency WHERE created_at < ?", t.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
