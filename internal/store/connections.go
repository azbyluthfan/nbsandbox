// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Secret modes: how a connection's credential is kept.
const (
	SecretNone       = "none"       // no credential (an open API, say)
	SecretManaged    = "managed"    // written by nbsandbox to its provider
	SecretReferenced = "referenced" // an existing secret the provider holds
)

var (
	// ErrConnectionName means the owner already has a connection by that name.
	ErrConnectionName = errors.New("you already have a connection with that name")
	// ErrConnectionInUse means a space still binds the connection.
	ErrConnectionInUse = errors.New("the connection is used by a space: unbind it first")
)

// Connection is a user's data source. Settings is JSON (host, port,
// database...); the credential is never stored here, only where its
// provider keeps it (SecretRef).
type Connection struct {
	ID, Owner, Name, Type string
	Settings              string // JSON object
	SecretMode            string // SecretNone, SecretManaged or SecretReferenced
	SecretProvider        string
	SecretRef             string
	CreatedAt, UpdatedAt  time.Time
}

const connectionCols = "id, owner, name, type, settings, secret_mode, secret_provider, secret_ref, created_at, updated_at"

func scanConnection(sc interface{ Scan(...any) error }) (Connection, error) {
	var c Connection
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.Owner, &c.Name, &c.Type, &c.Settings, &c.SecretMode, &c.SecretProvider, &c.SecretRef, &created, &updated); err != nil {
		return Connection{}, err
	}
	c.CreatedAt, c.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return c, nil
}

// InsertConnection records a new connection; ErrConnectionName if the owner
// already has one by that name.
func (db *DB) InsertConnection(ctx context.Context, c Connection) error {
	_, err := db.w.ExecContext(ctx, "INSERT INTO connections ("+connectionCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		c.ID, c.Owner, c.Name, c.Type, c.Settings, c.SecretMode, c.SecretProvider, c.SecretRef, c.CreatedAt.UnixMilli(), c.UpdatedAt.UnixMilli())
	if uniqueViolation(err, "connections_owner_name") {
		return ErrConnectionName
	}
	return err
}

// UpdateConnection rewrites an owner's connection (its ID and owner stay);
// ErrNotFound if the owner has no such connection, ErrConnectionName if the
// new name is taken.
func (db *DB) UpdateConnection(ctx context.Context, c Connection) error {
	res, err := db.w.ExecContext(ctx, `UPDATE connections SET name = ?, settings = ?, secret_mode = ?, secret_provider = ?, secret_ref = ?, updated_at = ?
		WHERE id = ? AND owner = ?`, c.Name, c.Settings, c.SecretMode, c.SecretProvider, c.SecretRef, c.UpdatedAt.UnixMilli(), c.ID, c.Owner)
	if uniqueViolation(err, "connections_owner_name") {
		return ErrConnectionName
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetConnection returns one of the owner's connections; ErrNotFound if the
// owner has none with that ID (another owner's is not found either).
func (db *DB) GetConnection(ctx context.Context, owner, id string) (Connection, error) {
	c, err := scanConnection(db.r.QueryRowContext(ctx, "SELECT "+connectionCols+" FROM connections WHERE id = ? AND owner = ?", id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrNotFound
	}
	return c, err
}

// ListConnections returns the owner's connections, by name.
func (db *DB) ListConnections(ctx context.Context, owner string) ([]Connection, error) {
	rows, err := db.r.QueryContext(ctx, "SELECT "+connectionCols+" FROM connections WHERE owner = ? ORDER BY name, id", owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConnection removes one of the owner's connections; ErrNotFound if
// there is none, ErrConnectionInUse while a space binds it.
func (db *DB) DeleteConnection(ctx context.Context, owner, id string) error {
	res, err := db.w.ExecContext(ctx, "DELETE FROM connections WHERE id = ? AND owner = ?", id, owner)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" { // foreign key: a binding refers to it
		return ErrConnectionInUse
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ConnectionUse is one place a connection is bound.
type ConnectionUse struct {
	SpaceID, SpaceName, Environment, Name string
}

// ConnectionUses lists where a connection is bound.
func (db *DB) ConnectionUses(ctx context.Context, id string) ([]ConnectionUse, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT b.space_id, s.name, b.environment, b.name FROM space_bindings b JOIN spaces s ON s.id = b.space_id
		WHERE b.connection_id = ? ORDER BY s.name, b.space_id, b.environment, b.name`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ConnectionUse
	for rows.Next() {
		var u ConnectionUse
		if err := rows.Scan(&u.SpaceID, &u.SpaceName, &u.Environment, &u.Name); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SpaceConnections is what names mean in a space: its environments, the
// default one, and each environment's bindings (name -> connection ID).
type SpaceConnections struct {
	Default      string
	Environments map[string]map[string]string
}

// GetSpaceConnections returns a space's environments and bindings (none:
// an empty SpaceConnections).
func (db *DB) GetSpaceConnections(ctx context.Context, spaceID string) (SpaceConnections, error) {
	out := SpaceConnections{Environments: map[string]map[string]string{}}
	rows, err := db.r.QueryContext(ctx, "SELECT name, is_default FROM space_environments WHERE space_id = ?", spaceID)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var def bool
		if err := rows.Scan(&name, &def); err != nil {
			return out, err
		}
		out.Environments[name] = map[string]string{}
		if def {
			out.Default = name
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	brows, err := db.r.QueryContext(ctx, "SELECT environment, name, connection_id FROM space_bindings WHERE space_id = ?", spaceID)
	if err != nil {
		return out, err
	}
	defer func() { _ = brows.Close() }()
	for brows.Next() {
		var env, name, conn string
		if err := brows.Scan(&env, &name, &conn); err != nil {
			return out, err
		}
		if out.Environments[env] != nil {
			out.Environments[env][name] = conn
		}
	}
	return out, brows.Err()
}

// SetSpaceConnections replaces a space's environments and bindings at once
// (the caller has checked them); ErrNotFound if a connection is gone.
func (db *DB) SetSpaceConnections(ctx context.Context, spaceID string, sc SpaceConnections) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM space_environments WHERE space_id = ?", spaceID); err != nil {
		return err
	}
	for env, binds := range sc.Environments {
		if _, err := tx.ExecContext(ctx, "INSERT INTO space_environments (space_id, name, is_default) VALUES (?, ?, ?)", spaceID, env, env == sc.Default); err != nil {
			return err
		}
		for name, conn := range binds {
			_, err := tx.ExecContext(ctx, "INSERT INTO space_bindings (space_id, environment, name, connection_id) VALUES (?, ?, ?, ?)", spaceID, env, name, conn)
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23503" {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// PutBuiltinSecret stores (or replaces) an encrypted value of the built-in
// secret store.
func (db *DB) PutBuiltinSecret(ctx context.Context, id string, nonce, ciphertext []byte, at time.Time) error {
	_, err := db.w.ExecContext(ctx, `INSERT INTO builtin_secrets (id, nonce, ciphertext, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET nonce = EXCLUDED.nonce, ciphertext = EXCLUDED.ciphertext, created_at = EXCLUDED.created_at`,
		id, nonce, ciphertext, at.UnixMilli())
	return err
}

// GetBuiltinSecret returns an encrypted value; ErrNotFound if there is none.
func (db *DB) GetBuiltinSecret(ctx context.Context, id string) (nonce, ciphertext []byte, err error) {
	err = db.r.QueryRowContext(ctx, "SELECT nonce, ciphertext FROM builtin_secrets WHERE id = ?", id).Scan(&nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	return nonce, ciphertext, err
}

// DeleteBuiltinSecret removes an encrypted value (none: no error).
func (db *DB) DeleteBuiltinSecret(ctx context.Context, id string) error {
	_, err := db.w.ExecContext(ctx, "DELETE FROM builtin_secrets WHERE id = ?", id)
	return err
}

// RunConnections returns which connection each of a run's names meant
// (name -> connection id), as resolved when it was submitted.
func (db *DB) RunConnections(ctx context.Context, runID string) (map[string]string, error) {
	rows, err := db.r.QueryContext(ctx, "SELECT name, connection_id FROM run_connections WHERE run_id = ?", runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var n, id string
		if err := rows.Scan(&n, &id); err != nil {
			return nil, err
		}
		out[n] = id
	}
	return out, rows.Err()
}

// ConnectionByID returns a connection whoever owns it (run start: the run
// already settled whose it is).
func (db *DB) ConnectionByID(ctx context.Context, id string) (Connection, error) {
	return scanConnection(db.r.QueryRowContext(ctx, "SELECT "+connectionCols+" FROM connections WHERE id = ?", id))
}
