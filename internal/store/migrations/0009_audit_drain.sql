-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- The audit log: who did what, when, and how it ended. One row per audited
-- operation, refused ones included.
CREATE TABLE audit_events (
	id       BIGSERIAL PRIMARY KEY,
	at       BIGINT NOT NULL,  -- unix ms
	actor    TEXT NOT NULL,    -- the caller ID
	method   TEXT NOT NULL,    -- basic or oidc
	client   TEXT NOT NULL,    -- the token's application, if any
	via      TEXT NOT NULL,    -- rest, grpc or ui
	action   TEXT NOT NULL,    -- the operation (gRPC method name)
	space_id TEXT NOT NULL,
	target   TEXT NOT NULL,    -- a path, run, function or version
	detail   TEXT NOT NULL,
	status   INTEGER NOT NULL, -- HTTP status (gRPC codes mapped)
	address  TEXT NOT NULL     -- the client's address
);
CREATE INDEX audit_events_at ON audit_events (at);
CREATE INDEX audit_events_actor ON audit_events (actor, id);
CREATE INDEX audit_events_space ON audit_events (space_id, id);

-- Draining: while set, no worker starts a queued run (the claim query
-- checks it). One row.
CREATE TABLE drain (
	id       INTEGER PRIMARY KEY CHECK (id = 1),
	draining BOOLEAN NOT NULL,
	reason   TEXT NOT NULL,
	by_whom  TEXT NOT NULL,
	since    BIGINT NOT NULL -- unix ms; 0 when not draining
);
INSERT INTO drain (id, draining, reason, by_whom, since) VALUES (1, false, '', '', 0);
