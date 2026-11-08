// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is the metadata store, in PostgreSQL. Several nbsandboxd processes can
// share one database: every rule that needs it (one writing run per space,
// event order, version numbers) is enforced by the database itself.
type DB struct {
	w pool // the pool; w and r are the same, named for what a call does
	r pool

	url    string // for the notification listener's own connection; never logged
	prefix string // of notification channel names: per schema, so tests don't hear each other
}

// Open connects to the database at url (postgres://user:pass@host/db...)
// and applies pending migrations. A search_path parameter in the URL selects
// a schema, which is created if needed (tests use one schema each).
func Open(ctx context.Context, dbURL string) (*DB, error) {
	u, err := url.Parse(dbURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, errors.New("database_url must be a postgres:// URL")
	}
	sqldb, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqldb.SetMaxOpenConns(20)
	sqldb.SetMaxIdleConns(5)
	sqldb.SetConnMaxIdleTime(5 * time.Minute)
	p := pool{sqldb}
	db := &DB{w: p, r: p, url: dbURL, prefix: "nbsandbox"}
	if schema := u.Query().Get("search_path"); schema != "" {
		db.prefix = "nbsandbox_" + schema
		if !schemaRE.MatchString(schema) {
			_ = sqldb.Close()
			return nil, fmt.Errorf("database_url: search_path %q must be a plain lowercase name", schema)
		}
		if _, err := sqldb.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
			_ = sqldb.Close()
			return nil, fmt.Errorf("create schema %s: %w", schema, err)
		}
	}
	if err := db.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

var schemaRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Close closes the pool.
func (db *DB) Close() error { return db.w.Close() }

// Ping checks the database is usable (readiness check "db").
func (db *DB) Ping(ctx context.Context) error {
	var n int
	return db.r.QueryRowContext(ctx, "SELECT 1").Scan(&n)
}

// SchemaVersion returns the highest applied migration.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := db.r.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&v)
	return v, err
}

// migrationLock serialises migrations between nbsandboxd processes starting
// at the same time (a transaction-level advisory lock).
const migrationLock = 7_310_001

func (db *DB) migrate(ctx context.Context) error {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	type mig struct {
		version int
		name    string
	}
	var migs []mig
	for _, f := range files {
		base := strings.TrimPrefix(f, "migrations/")
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", base)
		}
		migs = append(migs, mig{v, f})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", migrationLock); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL)"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var current int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&current); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for _, m := range migs {
		if m.version <= current {
			continue
		}
		body, err := migrationFS.ReadFile(m.name)
		if err != nil {
			return err
		}
		// Migration files are run as written: no placeholders to rewrite.
		if _, err := tx.Tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_version(version, applied_at) VALUES (?, ?)",
			m.version, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return tx.Commit()
}

// uniqueViolation reports whether err is a unique-constraint violation on
// the named constraint or index ("" = any).
func uniqueViolation(err error, constraint string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && (constraint == "" || pe.ConstraintName == constraint)
}

// The store's queries use "?" placeholders; PostgreSQL wants $1, $2, ...
// pool and txn rewrite them, so every query is written one way.

type pool struct{ *sql.DB }

func (p pool) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return p.DB.ExecContext(ctx, rebind(q), args...)
}

func (p pool) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return p.DB.QueryContext(ctx, rebind(q), args...)
}

func (p pool) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return p.DB.QueryRowContext(ctx, rebind(q), args...)
}

func (p pool) BeginTx(ctx context.Context, opts *sql.TxOptions) (txn, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	return txn{tx}, err
}

type txn struct{ *sql.Tx }

func (t txn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, rebind(q), args...)
}

func (t txn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, rebind(q), args...)
}

func (t txn) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, rebind(q), args...)
}

// rebind turns each "?" outside single-quoted literals into $1, $2, ...
func rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n, quoted := 0, false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			quoted = !quoted
			b.WriteByte(c)
		case c == '?' && !quoted:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
