// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UILogin is a sign-in to the web UI waiting for the identity provider's
// answer.
type UILogin struct {
	StateHash []byte
	Verifier  string // PKCE code_verifier
	Nonce     string
	ReturnTo  string // a path in the UI
	ExpiresAt time.Time
}

// UISession is a signed-in browser.
type UISession struct {
	IDHash        []byte
	Method        string // "basic" or "oidc"
	Principal     string // the caller ID
	DisplayName   string
	Tokens        []byte // encrypted by the caller; empty for basic
	AccessExpires time.Time
	CreatedAt     time.Time
	LastSeenAt    time.Time
}

// CreateUILogin records a sign-in in progress.
func (db *DB) CreateUILogin(ctx context.Context, l UILogin) error {
	_, err := db.w.ExecContext(ctx, `INSERT INTO ui_logins(state_hash, verifier, nonce, return_to, expires_at) VALUES (?, ?, ?, ?, ?)`,
		l.StateHash, l.Verifier, l.Nonce, l.ReturnTo, l.ExpiresAt.UnixMilli())
	return err
}

// TakeUILogin removes and returns the sign-in with this state, if it hasn't
// expired by now: each can be used once. ErrNotFound otherwise.
func (db *DB) TakeUILogin(ctx context.Context, stateHash []byte, now time.Time) (UILogin, error) {
	var l UILogin
	var exp int64
	err := db.w.QueryRowContext(ctx, `DELETE FROM ui_logins WHERE state_hash = ? RETURNING state_hash, verifier, nonce, return_to, expires_at`,
		stateHash).Scan(&l.StateHash, &l.Verifier, &l.Nonce, &l.ReturnTo, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return UILogin{}, ErrNotFound
	}
	if err != nil {
		return UILogin{}, err
	}
	l.ExpiresAt = time.UnixMilli(exp)
	if !now.Before(l.ExpiresAt) {
		return UILogin{}, ErrNotFound
	}
	return l, nil
}

const uiSessionCols = "id_hash, method, principal, display_name, tokens, access_expires, created_at, last_seen_at"

// CreateUISession records a signed-in browser.
func (db *DB) CreateUISession(ctx context.Context, s UISession) error {
	_, err := db.w.ExecContext(ctx, `INSERT INTO ui_sessions(`+uiSessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.IDHash, s.Method, s.Principal, s.DisplayName, s.Tokens, ms(s.AccessExpires), s.CreatedAt.UnixMilli(), s.LastSeenAt.UnixMilli())
	return err
}

// UISession returns a session; ErrNotFound if there is none.
func (db *DB) UISession(ctx context.Context, idHash []byte) (UISession, error) {
	var s UISession
	var acc, created, seen int64
	err := db.r.QueryRowContext(ctx, `SELECT `+uiSessionCols+` FROM ui_sessions WHERE id_hash = ?`, idHash).Scan(
		&s.IDHash, &s.Method, &s.Principal, &s.DisplayName, &s.Tokens, &acc, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return UISession{}, ErrNotFound
	}
	if err != nil {
		return UISession{}, err
	}
	if acc != 0 {
		s.AccessExpires = time.UnixMilli(acc)
	}
	s.CreatedAt, s.LastSeenAt = time.UnixMilli(created), time.UnixMilli(seen)
	return s, nil
}

// TouchUISession records that the session was used at `at`.
func (db *DB) TouchUISession(ctx context.Context, idHash []byte, at time.Time) error {
	_, err := db.w.ExecContext(ctx, `UPDATE ui_sessions SET last_seen_at = GREATEST(last_seen_at, ?) WHERE id_hash = ?`, at.UnixMilli(), idHash)
	return err
}

// UpdateUISessionTokens stores refreshed tokens.
func (db *DB) UpdateUISessionTokens(ctx context.Context, idHash, tokens []byte, accessExpires time.Time) error {
	_, err := db.w.ExecContext(ctx, `UPDATE ui_sessions SET tokens = ?, access_expires = ? WHERE id_hash = ?`, tokens, ms(accessExpires), idHash)
	return err
}

// DeleteUISession ends a session (no error if it is already gone).
func (db *DB) DeleteUISession(ctx context.Context, idHash []byte) error {
	_, err := db.w.ExecContext(ctx, `DELETE FROM ui_sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteExpiredUI removes sign-ins that expired before now, and sessions
// last used before idleBefore or created before createdBefore.
func (db *DB) DeleteExpiredUI(ctx context.Context, now, idleBefore, createdBefore time.Time) (int64, error) {
	if _, err := db.w.ExecContext(ctx, `DELETE FROM ui_logins WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return 0, err
	}
	res, err := db.w.ExecContext(ctx, `DELETE FROM ui_sessions WHERE last_seen_at < ? OR created_at < ?`,
		idleBefore.UnixMilli(), createdBefore.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
