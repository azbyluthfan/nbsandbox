// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"cmp"
	"context"
	"time"
)

// Worker is what a worker process publishes about itself.
type Worker struct {
	ID       string
	Up       bool   // can run notebooks now
	Runtime  string // JSON (GET /v1/runtime as this worker sees it)
	Manifest string // the runner image's dependency manifest, as labelled
	// SessionAddr is where API nodes reach its interactive sessions
	// (host:port), "" when it holds none.
	SessionAddr string
	SeenAt      time.Time
	// For the compute view: the process's role (worker, or all), its
	// version and start, and how many runs and sessions it takes at once.
	Role         string
	Version      string
	StartedAt    time.Time
	RunSlots     int
	SessionSlots int
	// Usage sums what its runs and sessions use now (JSON; "{}" before the
	// first sample).
	Usage string
}

// PutWorker records a worker's state, seen now.
func (db *DB) PutWorker(ctx context.Context, w Worker) error {
	_, err := db.w.ExecContext(ctx, `
		INSERT INTO workers(id, up, runtime, manifest, session_addr, role, version, started_at, run_slots, session_slots, usage, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+dbNowMS+`)
		ON CONFLICT (id) DO UPDATE SET up = EXCLUDED.up, runtime = EXCLUDED.runtime,
			manifest = EXCLUDED.manifest, session_addr = EXCLUDED.session_addr, role = EXCLUDED.role,
			version = EXCLUDED.version, started_at = EXCLUDED.started_at, run_slots = EXCLUDED.run_slots,
			session_slots = EXCLUDED.session_slots, usage = EXCLUDED.usage, seen_at = EXCLUDED.seen_at`,
		w.ID, w.Up, w.Runtime, w.Manifest, w.SessionAddr, w.Role, w.Version, w.StartedAt.UnixMilli(), w.RunSlots, w.SessionSlots, cmp.Or(w.Usage, "{}"))
	return err
}

// LiveWorkers returns the workers seen within the last `within`, those that
// can run notebooks first, then the most recently seen.
func (db *DB) LiveWorkers(ctx context.Context, within time.Duration) ([]Worker, error) {
	return db.workers(ctx, "WHERE seen_at > "+dbNowMS+" - ? ORDER BY up DESC, seen_at DESC, id", within.Milliseconds())
}

// AllWorkers returns every worker on record, by name: the live ones and
// those gone within the last day (DeleteWorker forgets older ones).
func (db *DB) AllWorkers(ctx context.Context) ([]Worker, error) {
	return db.workers(ctx, "ORDER BY id")
}

func (db *DB) workers(ctx context.Context, where string, args ...any) ([]Worker, error) {
	rows, err := db.r.QueryContext(ctx, `
		SELECT id, up, runtime, manifest, session_addr, seen_at, role, version, started_at, run_slots, session_slots, usage
		FROM workers `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Worker
	for rows.Next() {
		var w Worker
		var seen, started int64
		if err := rows.Scan(&w.ID, &w.Up, &w.Runtime, &w.Manifest, &w.SessionAddr, &seen, &w.Role, &w.Version, &started,
			&w.RunSlots, &w.SessionSlots, &w.Usage); err != nil {
			return nil, err
		}
		w.SeenAt = time.UnixMilli(seen)
		if started > 0 {
			w.StartedAt = time.UnixMilli(started)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkerLoad is what a worker holds now.
type WorkerLoad struct {
	Runs     int // claimed and not finished: starting or running
	Sessions int // open
}

// WorkerLoads counts the active runs and open sessions by worker. A single
// process (role all) holds its sessions without naming itself: they are
// under "".
func (db *DB) WorkerLoads(ctx context.Context) (map[string]WorkerLoad, error) {
	out := map[string]WorkerLoad{}
	count := func(q string, add func(*WorkerLoad, int)) error {
		rows, err := db.r.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				return err
			}
			l := out[id]
			add(&l, n)
			out[id] = l
		}
		return rows.Err()
	}
	if err := count(`SELECT worker, count(*) FROM runs WHERE worker <> '' AND status IN ('queued', 'running') GROUP BY worker`,
		func(l *WorkerLoad, n int) { l.Runs = n }); err != nil {
		return nil, err
	}
	if err := count(`SELECT worker, count(*) FROM sessions WHERE status <> 'ended' GROUP BY worker`,
		func(l *WorkerLoad, n int) { l.Sessions = n }); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteWorker forgets a worker (it is shutting down), and long-gone ones.
func (db *DB) DeleteWorker(ctx context.Context, id string, olderThan time.Duration) error {
	_, err := db.w.ExecContext(ctx, "DELETE FROM workers WHERE id = ? OR seen_at < "+dbNowMS+" - ?", id, olderThan.Milliseconds())
	return err
}
