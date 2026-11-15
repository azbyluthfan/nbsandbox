-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Signing in to the web UI. A sign-in with the identity provider in
-- progress: its PKCE verifier and nonce, used once by the callback (on any
-- node). Keyed by the SHA-256 of its state value.
CREATE TABLE ui_logins (
	state_hash BYTEA PRIMARY KEY,
	verifier   TEXT NOT NULL,
	nonce      TEXT NOT NULL,
	return_to  TEXT NOT NULL,
	expires_at BIGINT NOT NULL -- unix ms
);
CREATE INDEX ui_logins_expires ON ui_logins (expires_at);

-- A signed-in browser. Keyed by the SHA-256 of its cookie, so reading the
-- table doesn't give anyone a session. tokens: the provider's tokens,
-- encrypted (empty for a users.yaml sign-in).
CREATE TABLE ui_sessions (
	id_hash        BYTEA PRIMARY KEY,
	method         TEXT NOT NULL,   -- basic or oidc
	principal      TEXT NOT NULL,   -- the caller ID: the owner of their spaces
	display_name   TEXT NOT NULL,
	tokens         BYTEA NOT NULL,
	access_expires BIGINT NOT NULL, -- unix ms; 0 for basic
	created_at     BIGINT NOT NULL,
	last_seen_at   BIGINT NOT NULL
);
CREATE INDEX ui_sessions_last_seen ON ui_sessions (last_seen_at);
CREATE INDEX ui_sessions_created ON ui_sessions (created_at);
