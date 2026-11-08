// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrBadCursor means a pagination cursor could not be decoded.
var ErrBadCursor = errors.New("invalid cursor")

// GetSpace returns the space only if owner owns it. A space owned by someone
// else is reported as ErrNotFound, exactly like a missing one.
func (db *DB) GetSpace(ctx context.Context, owner, id string) (Space, error) {
	var sp Space
	var created, updated int64
	err := db.r.QueryRowContext(ctx,
		"SELECT id, owner, name, created_at, updated_at FROM spaces WHERE id = ? AND owner = ?", id, owner).
		Scan(&sp.ID, &sp.Owner, &sp.Name, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Space{}, ErrNotFound
	}
	if err != nil {
		return Space{}, err
	}
	sp.CreatedAt, sp.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return sp, nil
}

// SpacePage is one page of ListSpaces.
type SpacePage struct {
	Items      []Space
	NextCursor string // "" when there are no more
}

// ListSpaces returns owner's spaces, newest first, with stable cursor paging.
func (db *DB) ListSpaces(ctx context.Context, owner, cursor string, limit int) (SpacePage, error) {
	return db.listSpaces(ctx, &owner, cursor, limit)
}

// ListAllSpaces is ListSpaces for an administrator: every owner's spaces,
// or one owner's when owner isn't "".
func (db *DB) ListAllSpaces(ctx context.Context, owner, cursor string, limit int) (SpacePage, error) {
	if owner == "" {
		return db.listSpaces(ctx, nil, cursor, limit)
	}
	return db.listSpaces(ctx, &owner, cursor, limit)
}

func (db *DB) listSpaces(ctx context.Context, owner *string, cursor string, limit int) (SpacePage, error) {
	q := "SELECT id, owner, name, created_at, updated_at FROM spaces WHERE TRUE"
	var args []any
	if owner != nil {
		q += " AND owner = ?"
		args = append(args, *owner)
	}
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return SpacePage{}, err
		}
		q += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, at, at, id)
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return SpacePage{}, err
	}
	defer func() { _ = rows.Close() }()
	var page SpacePage
	for rows.Next() {
		var sp Space
		var created, updated int64
		if err := rows.Scan(&sp.ID, &sp.Owner, &sp.Name, &created, &updated); err != nil {
			return SpacePage{}, err
		}
		sp.CreatedAt, sp.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		page.Items = append(page.Items, sp)
	}
	if err := rows.Err(); err != nil {
		return SpacePage{}, err
	}
	if len(page.Items) > limit {
		last := page.Items[limit-1]
		page.Items = page.Items[:limit]
		page.NextCursor = encodeCursor(last.CreatedAt.UnixMilli(), last.ID)
	}
	return page, nil
}

// RenameSpace changes a space's name.
func (db *DB) RenameSpace(ctx context.Context, owner, id, name string, now time.Time) error {
	res, err := db.w.ExecContext(ctx,
		"UPDATE spaces SET name = ?, updated_at = ? WHERE id = ? AND owner = ?", name, now.UnixMilli(), id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSpace removes the space and its run rows, unless a run is queued or
// running (ErrActiveRun). The check and the delete are one statement, so a
// run can't slip in between.
func (db *DB) DeleteSpace(ctx context.Context, owner, id string) error {
	res, err := db.w.ExecContext(ctx, `
		DELETE FROM spaces WHERE id = ? AND owner = ?
		AND NOT EXISTS (SELECT 1 FROM runs WHERE space_id = ? AND status IN ('queued', 'running'))`,
		id, owner, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	if _, err := db.GetSpace(ctx, owner, id); err != nil {
		return err // ErrNotFound
	}
	return ErrActiveRun
}

// CountRuns returns how many runs a space has (any status).
func (db *DB) CountRuns(ctx context.Context, spaceID string) (int, error) {
	var n int
	err := db.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE space_id = ?", spaceID).Scan(&n)
	return n, err
}

// SpaceIDs returns every space ID (for startup consistency checks).
func (db *DB) SpaceIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := db.r.QueryContext(ctx, "SELECT id FROM spaces")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// Cursors are opaque to clients: base64url("<created_at ms>:<id>").
func encodeCursor(at int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10) + ":" + id))
}

func decodeCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", ErrBadCursor
	}
	at, id, ok := strings.Cut(string(b), ":")
	if !ok || id == "" {
		return 0, "", ErrBadCursor
	}
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrBadCursor, err) //nolint:errorlint // only ErrBadCursor is matched
	}
	return n, id, nil
}

// ErrSpaceLimit: the owner already has as many spaces as allowed.
var ErrSpaceLimit = errors.New("space limit reached")

// CreateSpaceCapped inserts a space unless its owner already has maxSpaces
// (0: no limit). Two creates for one owner at the same moment are
// serialised, so the cap holds.
func (db *DB) CreateSpaceCapped(ctx context.Context, sp Space, maxSpaces int) error {
	if maxSpaces <= 0 {
		return db.CreateSpace(ctx, sp)
	}
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?, hashtext(?))", spaceCapLock, sp.Owner); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM spaces WHERE owner = ?", sp.Owner).Scan(&n); err != nil {
		return err
	}
	if n >= maxSpaces {
		return ErrSpaceLimit
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO spaces(id, owner, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		sp.ID, sp.Owner, sp.Name, sp.CreatedAt.UnixMilli(), sp.UpdatedAt.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// spaceCapLock is the advisory lock class of CreateSpaceCapped (per owner).
const spaceCapLock = 7_310_004

// SpaceOwner returns who owns a space.
func (db *DB) SpaceOwner(ctx context.Context, id string) (string, error) {
	var owner string
	err := db.r.QueryRowContext(ctx, "SELECT owner FROM spaces WHERE id = ?", id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner, err
}

// SetSpaceOwner gives a space to another owner (an administrator's action:
// a user removed from the identity provider, or an issuer change).
func (db *DB) SetSpaceOwner(ctx context.Context, id, owner string, now time.Time) error {
	res, err := db.w.ExecContext(ctx, "UPDATE spaces SET owner = ?, updated_at = ? WHERE id = ?", owner, now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
