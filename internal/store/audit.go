// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strconv"
	"time"
)

// AuditEvent is a row of the audit log.
type AuditEvent struct {
	ID      int64
	At      time.Time
	Actor   string
	Method  string
	Client  string
	Via     string
	Action  string
	SpaceID string
	Target  string
	Detail  string
	Status  int
	Address string
}

// AuditFilter narrows ListAudit; empty fields match everything.
type AuditFilter struct {
	Actor, SpaceID, Action string
	Since, Until           *time.Time
}

// InsertAudit appends a row to the audit log.
func (db *DB) InsertAudit(ctx context.Context, e AuditEvent) error {
	_, err := db.w.ExecContext(ctx, `INSERT INTO audit_events(at, actor, method, client, via, action, space_id, target, detail, status, address)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.At.UnixMilli(), e.Actor, e.Method, e.Client, e.Via, e.Action, e.SpaceID, e.Target, e.Detail, e.Status, e.Address)
	return err
}

// ListAudit returns audit rows, newest first. The cursor is the last page's
// last ID; "" starts at the newest.
func (db *DB) ListAudit(ctx context.Context, f AuditFilter, cursor string, limit int) ([]AuditEvent, string, error) {
	q := "SELECT id, at, actor, method, client, via, action, space_id, target, detail, status, address FROM audit_events WHERE true"
	var args []any
	add := func(clause string, v any) {
		q += " AND " + clause
		args = append(args, v)
	}
	if f.Actor != "" {
		add("actor = ?", f.Actor)
	}
	if f.SpaceID != "" {
		add("space_id = ?", f.SpaceID)
	}
	if f.Action != "" {
		add("action = ?", f.Action)
	}
	if f.Since != nil {
		add("at >= ?", f.Since.UnixMilli())
	}
	if f.Until != nil {
		add("at < ?", f.Until.UnixMilli())
	}
	if cursor != "" {
		before, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || before <= 0 {
			return nil, "", ErrBadCursor
		}
		add("id < ?", before)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Method, &e.Client, &e.Via, &e.Action, &e.SpaceID, &e.Target, &e.Detail, &e.Status, &e.Address); err != nil {
			return nil, "", err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = strconv.FormatInt(out[limit-1].ID, 10)
	}
	return out, next, nil
}

// DeleteAuditBefore removes rows older than t.
func (db *DB) DeleteAuditBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := db.w.ExecContext(ctx, "DELETE FROM audit_events WHERE at < ?", t.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Drain is whether workers are drained: no queued run starts.
type Drain struct {
	Draining bool
	Reason   string
	By       string
	Since    time.Time // zero when not draining
	Queued   int       // runs waiting
	Running  int       // runs executing
}

// GetDrain returns the drain state and how many runs are queued and running.
func (db *DB) GetDrain(ctx context.Context) (Drain, error) {
	var d Drain
	var since int64
	err := db.r.QueryRowContext(ctx, `SELECT draining, reason, by_whom, since,
		(SELECT count(*) FROM runs WHERE status = 'queued'), (SELECT count(*) FROM runs WHERE status = 'running')
		FROM drain WHERE id = 1`).Scan(&d.Draining, &d.Reason, &d.By, &since, &d.Queued, &d.Running)
	if since > 0 {
		d.Since = time.UnixMilli(since)
	}
	return d, err
}

// SetDrain drains (or resumes) the workers. Draining again keeps the first
// time it started.
func (db *DB) SetDrain(ctx context.Context, draining bool, reason, by string, at time.Time) error {
	if !draining {
		_, err := db.w.ExecContext(ctx, "UPDATE drain SET draining = false, reason = '', by_whom = ?, since = 0 WHERE id = 1", by)
		return err
	}
	_, err := db.w.ExecContext(ctx, `UPDATE drain SET draining = true, reason = ?, by_whom = ?,
		since = CASE WHEN draining THEN since ELSE ? END WHERE id = 1`, reason, by, at.UnixMilli())
	return err
}
