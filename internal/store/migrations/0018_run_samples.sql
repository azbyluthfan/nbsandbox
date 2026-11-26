-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run's resource use over its life (docs/plan-compute-view.md): sampled
-- by its worker, removed with the run.
CREATE TABLE run_samples (
  run_id         TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  at             BIGINT  NOT NULL, -- unix ms
  memory_bytes   BIGINT  NOT NULL, -- working set
  cpu_millicores INTEGER NOT NULL, -- CPU used since the previous sample, per second
  written_bytes  BIGINT,           -- written to disk since the run started; NULL: the backend can't say
  PRIMARY KEY (run_id, at)
);

-- What a worker's runs and sessions use now, summed (JSON), with each
-- publish.
ALTER TABLE workers ADD COLUMN usage TEXT NOT NULL DEFAULT '{}';
