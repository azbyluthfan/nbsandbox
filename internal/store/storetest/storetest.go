// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package storetest gives each test its own empty nbsandbox database: a
// fresh schema in the test PostgreSQL (started by `make test-db`), dropped
// when the test ends. Tests run in parallel without seeing each other's rows.
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" driver

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// DefaultURL is the test database `make test-db` starts.
const DefaultURL = "postgres://nbsandbox:nbsandbox@127.0.0.1:55432/nbsandbox_test?sslmode=disable" //nolint:gosec // the throwaway test container on loopback, not a real credential

// BaseURL is the test database: NBSANDBOX_TEST_DATABASE_URL, or DefaultURL.
func BaseURL() string {
	if u := os.Getenv("NBSANDBOX_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return DefaultURL
}

// URL returns a database URL whose search_path is a new, empty schema, and
// drops the schema when the test ends.
func URL(t testing.TB) string {
	t.Helper()
	base := BaseURL()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.PingContext(context.Background()); err != nil {
		_ = admin.Close()
		t.Fatalf("no test database at %s (start one with `make test-db`, or set NBSANDBOX_TEST_DATABASE_URL): %v", redact(base), err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = admin.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// Open returns an empty, migrated database for this test, closed when the
// test ends.
func Open(t testing.TB) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable URL)"
	}
	return u.Redacted()
}
