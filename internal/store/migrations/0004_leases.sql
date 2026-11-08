-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Workers take queued runs with a lease they renew while the run is active.
-- Every claim gets a new token; a worker's writes for a run (start it,
-- record its outcome, write back the space, record its outputs) only apply
-- while the run still has its token, so a worker that lost its lease can't
-- change anything. Lease times use the database's clock.
CREATE SEQUENCE run_tokens;
ALTER TABLE runs ADD COLUMN worker      TEXT   NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN lease_until BIGINT NOT NULL DEFAULT 0; -- unix ms
ALTER TABLE runs ADD COLUMN token       BIGINT NOT NULL DEFAULT 0;
CREATE INDEX runs_leased ON runs(lease_until) WHERE worker <> '' AND status IN ('queued', 'running');

-- Live log lines of active runs, so any API node can stream them. Written in
-- batches by the run's worker; after the run its log.txt in object storage
-- is the source, and the chunks go.
CREATE TABLE run_log_chunks (
  run_id    TEXT   NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  first_seq BIGINT NOT NULL,
  last_seq  BIGINT NOT NULL,
  text      TEXT   NOT NULL, -- the lines first_seq..last_seq, each ending in \n
  PRIMARY KEY (run_id, first_seq)
);

-- Workers say what they run (the runner image, its packages, limits) and
-- that they are alive, for API nodes that have no Docker of their own.
CREATE TABLE workers (
  id       TEXT PRIMARY KEY,
  up       BOOLEAN NOT NULL,  -- Docker reachable and the runner image usable
  runtime  TEXT    NOT NULL,  -- JSON: GET /v1/runtime as this worker sees it
  manifest TEXT    NOT NULL,  -- the runner image's dependency manifest (its label)
  seen_at  BIGINT  NOT NULL   -- unix ms, database clock
);
