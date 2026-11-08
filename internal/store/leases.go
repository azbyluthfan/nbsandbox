// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrFenced refuses a worker's write for a run whose lease it no longer
// holds (another node took over the run, or ended it).
var ErrFenced = errors.New("the run's lease is no longer held by this worker")

// dbNowMS is the database's clock in unix milliseconds: leases use it, so
// nodes' clocks don't have to agree.
const dbNowMS = "(extract(epoch from clock_timestamp()) * 1000)::bigint"

// ClaimRun gives the oldest unclaimed queued run to worker, with a lease of
// d and a new token. ErrNotFound when there is none. Concurrent claims never
// take the same run, and never wait for each other.
func (db *DB) ClaimRun(ctx context.Context, worker string, d time.Duration) (Run, error) {
	r, err := scanRun(db.w.QueryRowContext(ctx, `
		UPDATE runs SET worker = ?, lease_until = `+dbNowMS+` + ?, token = nextval('run_tokens')
		WHERE id = (
			SELECT id FROM runs WHERE status = 'queued' AND worker = ''
			ORDER BY queue_seq FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING `+runCols, worker, d.Milliseconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// Lease is a worker's hold on a run.
type Lease struct {
	RunID string
	Token int64
}

// RenewLeases extends the given leases by d and returns the runs whose lease
// is no longer held (their token changed or they ended): the worker must
// stop them.
func (db *DB) RenewLeases(ctx context.Context, leases []Lease, d time.Duration) ([]string, error) {
	if len(leases) == 0 {
		return nil, nil
	}
	ids := make([]string, len(leases))
	tokens := make([]int64, len(leases))
	for i, l := range leases {
		ids[i], tokens[i] = l.RunID, l.Token
	}
	rows, err := db.w.QueryContext(ctx, `
		UPDATE runs SET lease_until = `+dbNowMS+` + ?
		FROM unnest(?::text[], ?::bigint[]) AS l(id, token)
		WHERE runs.id = l.id AND runs.token = l.token AND runs.status IN ('queued', 'running')
		RETURNING runs.id`, d.Milliseconds(), ids, tokens)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	held := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		held[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var lost []string
	for _, id := range ids {
		if !held[id] {
			lost = append(lost, id)
		}
	}
	return lost, nil
}

// ReleaseRun puts a claimed run that never started back in the queue, if
// the lease is still the given one.
func (db *DB) ReleaseRun(ctx context.Context, runID string, token int64) (bool, error) {
	res, err := db.w.ExecContext(ctx, `
		UPDATE runs SET worker = '', lease_until = 0
		WHERE id = ? AND token = ? AND status = 'queued'`, runID, token)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if n == 1 {
		err = errors.Join(err, db.Notify(ctx, ChanQueued, ""))
	}
	return n == 1, err
}

// ExpiredLeases returns active runs whose worker stopped renewing the lease.
func (db *DB) ExpiredLeases(ctx context.Context) ([]Run, error) {
	return db.queryRuns(ctx, "SELECT "+runCols+` FROM runs
		WHERE worker <> '' AND status IN ('queued', 'running') AND lease_until < `+dbNowMS+`
		ORDER BY queue_seq`)
}

// CancelRequested returns which of the given runs have a cancel request.
func (db *DB) CancelRequested(ctx context.Context, runIDs []string) ([]string, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := db.r.QueryContext(ctx,
		"SELECT id FROM runs WHERE id = ANY(?::text[]) AND cancelled_by <> '' AND status IN ('queued', 'running')", runIDs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// QueueLength is the number of queued runs.
func (db *DB) QueueLength(ctx context.Context) (int, error) {
	var n int
	err := db.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE status = 'queued'").Scan(&n)
	return n, err
}

// QueuePosition is a queued run's 1-based place in the queue; 0 if it isn't
// queued.
func (db *DB) QueuePosition(ctx context.Context, runID string) (int, error) {
	var n int
	err := db.r.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs q, runs r
		WHERE r.id = ? AND r.status = 'queued' AND q.status = 'queued' AND q.queue_seq <= r.queue_seq`, runID).Scan(&n)
	return n, err
}

// fence checks, inside tx, that the run still has the lease token and is
// running, and holds the row until tx ends so the lease can't change in
// between.
func fence(ctx context.Context, tx txn, runID string, token int64) error {
	var one int
	err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM runs WHERE id = ? AND token = ? AND status = 'running' FOR SHARE", runID, token).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFenced
	}
	return err
}

// LogChunk is a batch of consecutive log lines.
type LogChunk struct {
	FirstSeq, LastSeq int64
	Text              string // the lines, each ending in "\n"
}

// Lines splits the chunk into its lines (without their "\n").
func (c LogChunk) Lines() []string { return strings.Split(strings.TrimSuffix(c.Text, "\n"), "\n") }

// AppendLogChunk records a batch of live log lines of a running run (only
// while the worker holds its lease) and notifies followers.
func (db *DB) AppendLogChunk(ctx context.Context, runID string, token int64, c LogChunk) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fence(ctx, tx, runID, token); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_log_chunks(run_id, first_seq, last_seq, text) VALUES (?, ?, ?, ?)
		ON CONFLICT (run_id, first_seq) DO NOTHING`, runID, c.FirstSeq, c.LastSeq, c.Text); err != nil {
		return err
	}
	if err := db.notifyTx(ctx, tx, ChanLogs, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// LogChunks returns a run's live log chunks with lines after `after`, in
// order.
func (db *DB) LogChunks(ctx context.Context, runID string, after int64) ([]LogChunk, error) {
	rows, err := db.r.QueryContext(ctx,
		"SELECT first_seq, last_seq, text FROM run_log_chunks WHERE run_id = ? AND last_seq > ? ORDER BY first_seq", runID, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LogChunk
	for rows.Next() {
		var c LogChunk
		if err := rows.Scan(&c.FirstSeq, &c.LastSeq, &c.Text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteLogChunks removes a finished run's live log chunks (its log.txt is
// stored).
func (db *DB) DeleteLogChunks(ctx context.Context, runID string) error {
	_, err := db.w.ExecContext(ctx, "DELETE FROM run_log_chunks WHERE run_id = ?", runID)
	return err
}

// TryExclusive runs fn if no other node holds the advisory lock key right
// now, and reports whether it ran. For periodic jobs one node is enough for.
func (db *DB) TryExclusive(ctx context.Context, key int64, fn func(context.Context)) (bool, error) {
	conn, err := db.w.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil || !got {
		return false, err
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key)
	}()
	fn(ctx)
	return true, nil
}
