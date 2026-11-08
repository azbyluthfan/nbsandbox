-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- The nbsandbox schema for PostgreSQL. Times are unix milliseconds (BIGINT),
-- as the Go side reads and writes them.

CREATE TABLE spaces (
  id         TEXT PRIMARY KEY,
  owner      TEXT NOT NULL,                  -- the caller ID: a username, or <prefix>:<claims> for a token
  name       TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE INDEX spaces_owner_created ON spaces(owner, created_at DESC, id DESC);

-- The order runs were queued in (the queue is FIFO by this).
CREATE SEQUENCE runs_queue_seq;

CREATE TABLE runs (
  id                  TEXT PRIMARY KEY,
  space_id            TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  owner               TEXT NOT NULL,
  notebook            TEXT NOT NULL,
  parameters_json     TEXT NOT NULL DEFAULT '{}',
  status              TEXT NOT NULL,         -- queued | running | <terminal status>
  error_kind          TEXT NOT NULL DEFAULT '',
  fault               TEXT NOT NULL DEFAULT '',
  message             TEXT NOT NULL DEFAULT '',
  created_at          BIGINT NOT NULL,
  started_at          BIGINT,
  finished_at         BIGINT,
  queue_seq           BIGINT NOT NULL DEFAULT nextval('runs_queue_seq'),
  dependency_warnings TEXT NOT NULL DEFAULT '', -- JSON array from the submit-time check, '' when none
  function            TEXT NOT NULL DEFAULT '', -- published function, '' for a plain run
  version             INTEGER NOT NULL DEFAULT 0,
  notebook_sha256     TEXT NOT NULL DEFAULT '', -- of the snapshot the run executes
  output_schema       TEXT NOT NULL DEFAULT '', -- the contract's output schema, '' if none
  writes_space        INTEGER NOT NULL DEFAULT 1, -- 0: /space mounted read-only
  cancelled_by        TEXT NOT NULL DEFAULT '',
  cancel_reason       TEXT NOT NULL DEFAULT '',
  output_bytes        BIGINT NOT NULL DEFAULT 0
);
-- At most one queued or running run that can write a space, enforced by the
-- database. Read-only runs (function calls) run side by side.
CREATE UNIQUE INDEX one_active_writer_per_space ON runs(space_id)
  WHERE status IN ('queued', 'running') AND writes_space = 1;
CREATE INDEX runs_space_created ON runs(space_id, created_at DESC, id DESC);
CREATE INDEX runs_status_seq ON runs(status, queue_seq);
CREATE INDEX runs_space_function ON runs(space_id, function, created_at DESC, id DESC);
CREATE INDEX runs_finished ON runs(finished_at) WHERE finished_at IS NOT NULL;

CREATE TABLE idempotency (
  owner      TEXT NOT NULL,
  key        TEXT NOT NULL,
  body_hash  TEXT NOT NULL,
  run_id     TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (owner, key)
);
CREATE INDEX idempotency_created ON idempotency(created_at);

CREATE TABLE functions (
  space_id       TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  name           TEXT NOT NULL,
  latest_version INTEGER NOT NULL DEFAULT 0,
  created_at     BIGINT NOT NULL,
  updated_at     BIGINT NOT NULL,
  PRIMARY KEY (space_id, name)
);

CREATE TABLE function_versions (
  space_id        TEXT NOT NULL,
  function        TEXT NOT NULL,
  version         INTEGER NOT NULL,
  notebook        TEXT NOT NULL,               -- path it was published from
  files_json      TEXT NOT NULL DEFAULT '[]',  -- other files in the bundle (paths)
  contract_input  TEXT NOT NULL DEFAULT '',
  contract_output TEXT NOT NULL DEFAULT '',
  notebook_sha256 TEXT NOT NULL,
  bundle_bytes    BIGINT NOT NULL,
  created_by      TEXT NOT NULL,
  created_at      BIGINT NOT NULL,
  PRIMARY KEY (space_id, function, version),
  FOREIGN KEY (space_id, function) REFERENCES functions(space_id, name) ON DELETE CASCADE
);

-- Status changes, numbered, for the SSE status streams and their replay.
-- Inserts take a transaction-level advisory lock (see insertEvent), so
-- numbers become visible in order: a reader never sees 42 before 41.
CREATE TABLE run_events (
  seq        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  space_id   TEXT NOT NULL,
  run_id     TEXT NOT NULL,
  function   TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL,
  data       TEXT NOT NULL,                    -- the event body (JSON)
  created_at BIGINT NOT NULL
);
CREATE INDEX run_events_space ON run_events(space_id, seq);
CREATE INDEX run_events_run ON run_events(run_id, seq);
CREATE INDEX run_events_created ON run_events(created_at);
