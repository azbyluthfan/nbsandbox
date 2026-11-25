-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- The environment a run read its space's names in ('' if the space binds
-- none), and which connection each name meant when it was submitted
-- (docs/plan-data-access.md). No foreign key to connections: a run's
-- record outlives a connection deleted later; a queued run whose
-- connection is gone fails at start.
ALTER TABLE runs ADD COLUMN environment TEXT NOT NULL DEFAULT '';

CREATE TABLE run_connections (
  run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  connection_id TEXT NOT NULL,
  PRIMARY KEY (run_id, name)
);
