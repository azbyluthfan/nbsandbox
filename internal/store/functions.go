// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Function is a row in functions.
type Function struct {
	SpaceID, Name        string
	LatestVersion        int
	CreatedAt, UpdatedAt time.Time
}

// FunctionVersion is one published, immutable version.
type FunctionVersion struct {
	SpaceID, Function             string
	Version                       int
	Notebook                      string // path it was published from (and its path in the bundle)
	FilesJSON                     string // other files in the bundle, as a JSON array of paths
	ContractInput, ContractOutput string // JSON Schemas, "" if none (output: of the value, or of each item when streamed)
	ContractMode                  string // single | stream
	ContractModeDeclared          bool   // the contract named the mode (otherwise inferred from the notebook)
	NotebookSHA256                string
	BundleBytes                   int64
	CreatedBy                     string
	CreatedAt                     time.Time
	// Files is what the version holds (the notebook included): index rows,
	// recorded with the version; the contents are in object storage.
	Files []ContentRef
}

// NextFunctionVersion returns the number the next published version of a
// function gets (1 for a new function).
func (db *DB) NextFunctionVersion(ctx context.Context, spaceID, name string) (int, error) {
	var v sql.NullInt64
	err := db.r.QueryRowContext(ctx, "SELECT latest_version FROM functions WHERE space_id = ? AND name = ?", spaceID, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	return int(v.Int64) + 1, err
}

// AddFunctionVersion records a published version, creating the function on
// its first version. The version must be the next one: two publishes racing
// can't both get the same number.
func (db *DB) AddFunctionVersion(ctx context.Context, fv FunctionVersion) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	at := fv.CreatedAt.UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO functions(space_id, name, latest_version, created_at, updated_at) VALUES (?, ?, 0, ?, ?)
		ON CONFLICT(space_id, name) DO NOTHING`, fv.SpaceID, fv.Function, at, at); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE functions SET latest_version = ?, updated_at = ?
		WHERE space_id = ? AND name = ? AND latest_version = ?`, fv.Version, at, fv.SpaceID, fv.Function, fv.Version-1)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrVersionTaken
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO function_versions(space_id, function, version, notebook, files_json, contract_input, contract_output,
		                              contract_mode, contract_mode_declared, notebook_sha256, bundle_bytes, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fv.SpaceID, fv.Function, fv.Version, fv.Notebook, fv.FilesJSON, fv.ContractInput, fv.ContractOutput,
		modeOrSingle(fv.ContractMode), fv.ContractModeDeclared, fv.NotebookSHA256, fv.BundleBytes, fv.CreatedBy, at); err != nil {
		return err
	}
	for _, f := range fv.Files {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO function_version_files(space_id, function, version, path, sha256, size) VALUES (?, ?, ?, ?, ?, ?)`,
			fv.SpaceID, fv.Function, fv.Version, f.Path, f.SHA256, f.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ErrVersionTaken: another publish got this version number first.
var ErrVersionTaken = errors.New("that version number was just taken")

// Functions lists a space's functions by name.
func (db *DB) Functions(ctx context.Context, spaceID string) ([]Function, error) {
	rows, err := db.r.QueryContext(ctx,
		"SELECT space_id, name, latest_version, created_at, updated_at FROM functions WHERE space_id = ? ORDER BY name", spaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Function
	for rows.Next() {
		var f Function
		var c, u int64
		if err := rows.Scan(&f.SpaceID, &f.Name, &f.LatestVersion, &c, &u); err != nil {
			return nil, err
		}
		f.CreatedAt, f.UpdatedAt = time.UnixMilli(c), time.UnixMilli(u)
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFunction returns one function.
func (db *DB) GetFunction(ctx context.Context, spaceID, name string) (Function, error) {
	var f Function
	var c, u int64
	err := db.r.QueryRowContext(ctx,
		"SELECT space_id, name, latest_version, created_at, updated_at FROM functions WHERE space_id = ? AND name = ?",
		spaceID, name).Scan(&f.SpaceID, &f.Name, &f.LatestVersion, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return Function{}, ErrNotFound
	}
	f.CreatedAt, f.UpdatedAt = time.UnixMilli(c), time.UnixMilli(u)
	return f, err
}

const fvCols = "space_id, function, version, notebook, files_json, contract_input, contract_output, contract_mode, contract_mode_declared, " +
	"notebook_sha256, bundle_bytes, created_by, created_at"

func modeOrSingle(m string) string {
	if m == "" {
		return "single"
	}
	return m
}

func scanFV(s interface{ Scan(...any) error }) (FunctionVersion, error) {
	var fv FunctionVersion
	var at int64
	err := s.Scan(&fv.SpaceID, &fv.Function, &fv.Version, &fv.Notebook, &fv.FilesJSON, &fv.ContractInput, &fv.ContractOutput,
		&fv.ContractMode, &fv.ContractModeDeclared, &fv.NotebookSHA256, &fv.BundleBytes, &fv.CreatedBy, &at)
	fv.CreatedAt = time.UnixMilli(at)
	return fv, err
}

// FunctionVersions lists a function's versions, newest first.
func (db *DB) FunctionVersions(ctx context.Context, spaceID, name string) ([]FunctionVersion, error) {
	rows, err := db.r.QueryContext(ctx,
		"SELECT "+fvCols+" FROM function_versions WHERE space_id = ? AND function = ? ORDER BY version DESC", spaceID, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FunctionVersion
	for rows.Next() {
		fv, err := scanFV(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fv)
	}
	return out, rows.Err()
}

// GetFunctionVersion returns one version; version 0 means the latest.
func (db *DB) GetFunctionVersion(ctx context.Context, spaceID, name string, version int) (FunctionVersion, error) {
	q := "SELECT " + fvCols + " FROM function_versions WHERE space_id = ? AND function = ? AND version = ?"
	args := []any{spaceID, name, version}
	if version == 0 {
		q = "SELECT " + fvCols + " FROM function_versions WHERE space_id = ? AND function = ? ORDER BY version DESC LIMIT 1"
		args = args[:2]
	}
	fv, err := scanFV(db.r.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return FunctionVersion{}, ErrNotFound
	}
	return fv, err
}

// DeleteFunction removes a function and its versions, unless one of its
// invocations is still queued or running. Its past runs stay.
func (db *DB) DeleteFunction(ctx context.Context, spaceID, name string) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var active int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE space_id = ? AND function = ? AND status IN ('queued', 'running')",
		spaceID, name).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrActiveRun
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM functions WHERE space_id = ? AND name = ?", spaceID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
