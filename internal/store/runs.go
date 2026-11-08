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

// Errors returned by the store.
var (
	// ErrActiveRun means the space already has a queued or running run.
	ErrActiveRun = errors.New("space already has an active run")
	// ErrNotFound means the row does not exist.
	ErrNotFound = errors.New("not found")
)

// Space is a row in spaces.
type Space struct {
	ID, Owner, Name      string
	CreatedAt, UpdatedAt time.Time
}

// CreateSpace inserts a space.
func (db *DB) CreateSpace(ctx context.Context, sp Space) error {
	_, err := db.w.ExecContext(ctx,
		"INSERT INTO spaces(id, owner, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		sp.ID, sp.Owner, sp.Name, sp.CreatedAt.UnixMilli(), sp.UpdatedAt.UnixMilli())
	return err
}

// NewRun is what InsertRun needs.
type NewRun struct {
	ID, SpaceID, Owner, Notebook, ParametersJSON string
	DependencyWarnings                           string // JSON array or ""
	CreatedAt                                    time.Time
	Function                                     string
	Version                                      int
	NotebookSHA256                               string
	OutputSchema                                 string       // of the value, or of each item when streamed
	OutputMode                                   string       // single (default) | stream
	ClientApp                                    string       // the application that called on its user's behalf ('' if unknown)
	ReadOnlySpace                                bool         // /space mounted read-only; may run beside other runs
	Files                                        []ContentRef // the notebook snapshot and parameters, recorded with the run
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// InsertRun records a queued run. It returns ErrActiveRun if the space already
// has a queued or running run (the one_active_writer_per_space index).
func (db *DB) InsertRun(ctx context.Context, r NewRun) error {
	if r.ParametersJSON == "" {
		r.ParametersJSON = "{}"
	}
	_, err := db.w.ExecContext(ctx, `
		INSERT INTO runs(id, space_id, owner, notebook, parameters_json, status, created_at)
		VALUES (?, ?, ?, ?, ?, 'queued', ?)`,
		r.ID, r.SpaceID, r.Owner, r.Notebook, r.ParametersJSON, r.CreatedAt.UnixMilli())
	if uniqueViolation(err, "one_active_writer_per_space") {
		return ErrActiveRun
	}
	return err
}

// MarkRunning moves a queued run to running. It reports false if the run was
// no longer queued (e.g. it was cancelled while waiting).
func (db *DB) MarkRunning(ctx context.Context, runID string, at time.Time) (bool, error) {
	ok, _, err := db.MarkRunningWithEvent(ctx, runID, 0, at, nil)
	return ok, err
}

// MarkRunningWithEvent is MarkRunning that also records ev, in the same
// transaction, if the run moved. With a token, only the worker holding that
// lease can start the run.
func (db *DB) MarkRunningWithEvent(ctx context.Context, runID string, token int64, at time.Time, ev *Event) (bool, int64, error) {
	return db.transition(ctx, ev,
		"UPDATE runs SET status = 'running', started_at = ? WHERE id = ? AND status = 'queued' AND (? = 0 OR token = ?)",
		at.UnixMilli(), runID, token, token)
}

// Outcome is a run's terminal result.
type Outcome struct {
	Status, ErrorKind, Fault, Message string
	FinishedAt                        time.Time
	OutputBytes                       int64
}

// FinishRun moves a queued or running run to a terminal status in one
// conditional statement. It reports whether this caller won: when a
// cancel races a completion, exactly one FinishRun returns true and only that
// status is ever recorded.
func (db *DB) FinishRun(ctx context.Context, runID string, o Outcome) (bool, error) {
	ok, _, err := db.FinishRunWithEvent(ctx, runID, Fence{}, o, nil)
	return ok, err
}

// Fence narrows who may finish a run. The zero value: anyone, while the run
// is active.
type Fence struct {
	Token      int64 // only while the run still has this lease token
	QueuedOnly bool  // only while the run is queued (a cancel before it starts)
}

// FinishRunWithEvent is FinishRun that also records ev, in the same
// transaction, only if this caller won. No status change without its event,
// and no event without the change.
func (db *DB) FinishRunWithEvent(ctx context.Context, runID string, f Fence, o Outcome, ev *Event) (bool, int64, error) {
	if o.Status == "queued" || o.Status == "running" || o.Status == "" {
		return false, 0, fmt.Errorf("finish run: %q is not a terminal status", o.Status)
	}
	statuses := "'queued', 'running'"
	if f.QueuedOnly {
		statuses = "'queued'"
	}
	return db.transition(ctx, ev, `
		UPDATE runs SET status = ?, error_kind = ?, fault = ?, message = ?, finished_at = ?, output_bytes = ?
		WHERE id = ? AND status IN (`+statuses+`) AND (? = 0 OR token = ?)`,
		o.Status, o.ErrorKind, o.Fault, o.Message, o.FinishedAt.UnixMilli(), o.OutputBytes, runID, f.Token, f.Token)
}

// transition runs one conditional UPDATE and, if it changed the row, inserts
// ev, all in one transaction.
func (db *DB) transition(ctx context.Context, ev *Event, update string, args ...any) (bool, int64, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, update, args...)
	if err != nil {
		return false, 0, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, 0, err
	}
	var seq int64
	if ev != nil {
		if seq, err = db.insertEvent(ctx, tx, *ev); err != nil {
			return false, 0, err
		}
	}
	return true, seq, tx.Commit()
}

// RequestCancel records who cancelled an active run, and why, the first time
// it is asked. It reports false if the run is no longer active or was already
// being cancelled.
func (db *DB) RequestCancel(ctx context.Context, runID, by, reason string) (bool, error) {
	res, err := db.w.ExecContext(ctx, `
		UPDATE runs SET cancelled_by = ?, cancel_reason = ?
		WHERE id = ? AND status IN ('queued', 'running') AND cancelled_by = ''`, by, reason, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RunStatus returns a run's current status.
func (db *DB) RunStatus(ctx context.Context, runID string) (string, error) {
	var s string
	err := db.r.QueryRowContext(ctx, "SELECT status FROM runs WHERE id = ?", runID).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return s, err
}
