-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Interactive sessions: one notebook's live kernel, in a container on a
-- worker (docs/sessions.md).
CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  space_id   TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  owner      TEXT NOT NULL,
  notebook   TEXT NOT NULL,
  mode       TEXT NOT NULL CHECK (mode IN ('read-write', 'read-only')),
  status     TEXT NOT NULL CHECK (status IN ('starting', 'ready', 'ended')),
  worker     TEXT NOT NULL DEFAULT '', -- the worker it runs on; '' until one takes it
  created_at BIGINT NOT NULL,
  ended_at   BIGINT,
  end_reason TEXT NOT NULL DEFAULT '',
  ended_by   TEXT NOT NULL DEFAULT ''  -- who ended it ('' when it ended by itself)
);
-- One open session per notebook; at most one read-write session per space
-- (it holds the space's write lock: see also InsertRun).
CREATE UNIQUE INDEX one_open_session_per_notebook ON sessions(space_id, notebook) WHERE status <> 'ended';
CREATE UNIQUE INDEX one_writing_session_per_space ON sessions(space_id) WHERE status <> 'ended' AND mode = 'read-write';
CREATE INDEX sessions_open_owner ON sessions(owner) WHERE status <> 'ended';
CREATE INDEX sessions_open_worker ON sessions(worker) WHERE status <> 'ended';
CREATE INDEX sessions_ended ON sessions(ended_at) WHERE status = 'ended';
