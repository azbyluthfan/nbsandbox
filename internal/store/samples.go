// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"time"
)

// RunSample is a run's resource use at one moment.
type RunSample struct {
	At            time.Time
	MemoryBytes   int64
	CPUMillicores int
	WrittenBytes  *int64 // nil: the backend can't say
}

// MaxRunSamples bounds what RunSamples returns: a run sampled every 5 s for
// an hour, then every minute (docs/plan-compute-view.md), keeps about 1,300
// samples in ten hours.
const MaxRunSamples = 5000

// AddRunSample records one sample of a run.
func (db *DB) AddRunSample(ctx context.Context, runID string, s RunSample) error {
	var written sql.NullInt64
	if s.WrittenBytes != nil {
		written = sql.NullInt64{Int64: *s.WrittenBytes, Valid: true}
	}
	_, err := db.w.ExecContext(ctx, `INSERT INTO run_samples(run_id, at, memory_bytes, cpu_millicores, written_bytes)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, runID, s.At.UnixMilli(), s.MemoryBytes, s.CPUMillicores, written)
	return err
}

// RunSamples returns a run's samples, oldest first (MaxRunSamples at most).
func (db *DB) RunSamples(ctx context.Context, runID string) ([]RunSample, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT at, memory_bytes, cpu_millicores, written_bytes FROM run_samples
		WHERE run_id = ? ORDER BY at LIMIT ?`, runID, MaxRunSamples)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RunSample
	for rows.Next() {
		var s RunSample
		var at int64
		var written sql.NullInt64
		if err := rows.Scan(&at, &s.MemoryBytes, &s.CPUMillicores, &written); err != nil {
			return nil, err
		}
		s.At = time.UnixMilli(at)
		if written.Valid {
			s.WrittenBytes = &written.Int64
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
