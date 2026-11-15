// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PackageSet is a row of package_sets.
type PackageSet struct {
	Key          string
	Status       string // pending, building, ready, failed
	Requirements string // what to install: canonical requirements, or a lock
	PreferLock   string // a lock to try first ('' if none)
	FromLock     bool   // Requirements is a lock: install exactly it
	Lock         string // what was installed
	SizeBytes    int64
	Log          string
	Error        string
	Builder      string
	BuildUntil   time.Time
	CreatedAt    time.Time
	BuiltAt      time.Time // zero until finished
	LastUsed     time.Time
}

// The statuses of a package set.
const (
	PackagesPending  = "pending"
	PackagesBuilding = "building"
	PackagesReady    = "ready"
	PackagesFailed   = "failed"
)

const packageCols = "key, status, requirements, prefer_lock, from_lock, lock, size_bytes, log, error, builder, build_until, created_at, built_at, last_used"

func scanPackageSet(s interface{ Scan(...any) error }) (PackageSet, error) {
	var p PackageSet
	var until, created, built, used int64
	err := s.Scan(&p.Key, &p.Status, &p.Requirements, &p.PreferLock, &p.FromLock, &p.Lock, &p.SizeBytes, &p.Log, &p.Error, &p.Builder,
		&until, &created, &built, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return PackageSet{}, ErrNotFound
	}
	if err != nil {
		return PackageSet{}, err
	}
	p.BuildUntil, p.CreatedAt, p.LastUsed = time.UnixMilli(until), time.UnixMilli(created), time.UnixMilli(used)
	if built > 0 {
		p.BuiltAt = time.UnixMilli(built)
	}
	return p, nil
}

// GetPackageSet returns a set; ErrNotFound if nobody asked for it.
func (db *DB) GetPackageSet(ctx context.Context, key string) (PackageSet, error) {
	return scanPackageSet(db.r.QueryRowContext(ctx, "SELECT "+packageCols+" FROM package_sets WHERE key = ?", key))
}

// PackageRequest asks for a set.
type PackageRequest struct {
	Key          string
	Requirements string
	PreferLock   string
	FromLock     bool
}

// RequestPackageSet asks for the set: a new one is pending (a worker will
// build it); one that failed more than retryFailed ago is pending again. It
// returns the set as it is now.
func (db *DB) RequestPackageSet(ctx context.Context, r PackageRequest, retryFailed time.Duration, now time.Time) (PackageSet, error) {
	ms := now.UnixMilli()
	_, err := db.w.ExecContext(ctx, `
		INSERT INTO package_sets(key, status, requirements, prefer_lock, from_lock, lock, size_bytes, log, error, builder, build_until, created_at, built_at, last_used)
		VALUES (?, 'pending', ?, ?, ?, '', 0, '', '', '', 0, ?, 0, ?)
		ON CONFLICT (key) DO UPDATE SET status = 'pending', error = '', log = '', prefer_lock = EXCLUDED.prefer_lock
		WHERE package_sets.status = 'failed' AND package_sets.built_at < ?`,
		r.Key, r.Requirements, r.PreferLock, r.FromLock, ms, ms, now.Add(-retryFailed).UnixMilli())
	if err != nil {
		return PackageSet{}, err
	}
	return db.GetPackageSet(ctx, r.Key)
}

// ClaimPackageBuild gives worker the oldest pending set to build (or one
// whose builder's lease ran out), for lease. ErrNotFound when there is none.
func (db *DB) ClaimPackageBuild(ctx context.Context, worker string, lease time.Duration, now time.Time) (PackageSet, error) {
	return scanPackageSet(db.w.QueryRowContext(ctx, `
		UPDATE package_sets SET status = 'building', builder = ?, build_until = ?
		WHERE key = (
			SELECT key FROM package_sets
			WHERE status = 'pending' OR (status = 'building' AND build_until < ?)
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING `+packageCols, worker, now.Add(lease).UnixMilli(), now.UnixMilli()))
}

// RenewPackageBuild extends worker's lease on the build of key; false if it
// no longer holds it.
func (db *DB) RenewPackageBuild(ctx context.Context, key, worker string, until time.Time) (bool, error) {
	res, err := db.w.ExecContext(ctx, "UPDATE package_sets SET build_until = ? WHERE key = ? AND builder = ? AND status = 'building'",
		until.UnixMilli(), key, worker)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishPackageBuild records worker's build of key: ready with its lock and
// size, or failed with the reason. False if worker no longer held the build.
func (db *DB) FinishPackageBuild(ctx context.Context, key, worker string, ok bool, lock string, size int64, log, reason string, now time.Time) (bool, error) {
	status := PackagesFailed
	if ok {
		status = PackagesReady
	}
	res, err := db.w.ExecContext(ctx, `UPDATE package_sets SET status = ?, lock = ?, size_bytes = ?, log = ?, error = ?, built_at = ?, last_used = ?
		WHERE key = ? AND builder = ? AND status = 'building'`, status, lock, size, log, reason, now.UnixMilli(), now.UnixMilli(), key, worker)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// TouchPackageSet records that a run used the set.
func (db *DB) TouchPackageSet(ctx context.Context, key string, now time.Time) error {
	_, err := db.w.ExecContext(ctx, "UPDATE package_sets SET last_used = GREATEST(last_used, ?) WHERE key = ?", now.UnixMilli(), key)
	return err
}

// UnusedPackageSets are the keys of finished sets last used before t that no
// queued or running run waits for.
func (db *DB) UnusedPackageSets(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT key FROM package_sets p WHERE status IN ('ready', 'failed') AND last_used < ?
		AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.package_key = p.key AND r.status IN ('queued', 'running'))`, before.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeletePackageSet forgets a finished set (its files are removed by the
// caller).
func (db *DB) DeletePackageSet(ctx context.Context, key string) error {
	_, err := db.w.ExecContext(ctx, "DELETE FROM package_sets WHERE key = ? AND status IN ('ready', 'failed')", key)
	return err
}

// QueuedRunsWaitingFor are the queued runs waiting for the set key.
func (db *DB) QueuedRunsWaitingFor(ctx context.Context, key string) ([]Run, error) {
	return db.queryRuns(ctx, "SELECT "+runCols+" FROM runs WHERE package_key = ? AND status = 'queued' ORDER BY queue_seq", key)
}

// SpacePackages returns the space's last built requirements and lock.
func (db *DB) SpacePackages(ctx context.Context, spaceID string) (requirements, lock string, err error) {
	err = db.r.QueryRowContext(ctx, "SELECT package_requirements, package_lock FROM spaces WHERE id = ?", spaceID).Scan(&requirements, &lock)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return requirements, lock, err
}

// SetSpacePackages records the space's last built requirements and lock.
func (db *DB) SetSpacePackages(ctx context.Context, spaceID, requirements, lock string) error {
	_, err := db.w.ExecContext(ctx, "UPDATE spaces SET package_requirements = ?, package_lock = ? WHERE id = ?", requirements, lock, spaceID)
	return err
}
