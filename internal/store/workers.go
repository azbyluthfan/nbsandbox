// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"
)

// Worker is what a worker process publishes about itself.
type Worker struct {
	ID       string
	Up       bool   // can run notebooks now
	Runtime  string // JSON (GET /v1/runtime as this worker sees it)
	Manifest string // the runner image's dependency manifest, as labelled
	SeenAt   time.Time
}

// PutWorker records a worker's state, seen now.
func (db *DB) PutWorker(ctx context.Context, w Worker) error {
	_, err := db.w.ExecContext(ctx, `
		INSERT INTO workers(id, up, runtime, manifest, seen_at) VALUES (?, ?, ?, ?, `+dbNowMS+`)
		ON CONFLICT (id) DO UPDATE SET up = EXCLUDED.up, runtime = EXCLUDED.runtime,
			manifest = EXCLUDED.manifest, seen_at = EXCLUDED.seen_at`, w.ID, w.Up, w.Runtime, w.Manifest)
	return err
}

// LiveWorkers returns the workers seen within the last `within`, those that
// can run notebooks first, then the most recently seen.
func (db *DB) LiveWorkers(ctx context.Context, within time.Duration) ([]Worker, error) {
	rows, err := db.r.QueryContext(ctx, `
		SELECT id, up, runtime, manifest, seen_at FROM workers
		WHERE seen_at > `+dbNowMS+` - ? ORDER BY up DESC, seen_at DESC, id`, within.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Worker
	for rows.Next() {
		var w Worker
		var seen int64
		if err := rows.Scan(&w.ID, &w.Up, &w.Runtime, &w.Manifest, &seen); err != nil {
			return nil, err
		}
		w.SeenAt = time.UnixMilli(seen)
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWorker forgets a worker (it is shutting down), and long-gone ones.
func (db *DB) DeleteWorker(ctx context.Context, id string, olderThan time.Duration) error {
	_, err := db.w.ExecContext(ctx, "DELETE FROM workers WHERE id = ? OR seen_at < "+dbNowMS+" - ?", id, olderThan.Milliseconds())
	return err
}
