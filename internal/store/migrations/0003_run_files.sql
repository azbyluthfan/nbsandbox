-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run's files (notebook snapshot, parameters, log, executed notebook,
-- HTML, result, returned value, artifacts): contents in object storage under
-- their SHA-256, like space files. Uploaded when the run ends, so any node
-- can serve them; removed with the run.
CREATE TABLE run_files (
  run_id TEXT   NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  path   TEXT   NOT NULL,                  -- relative to the run's folder, e.g. artifacts/summary.csv
  sha256 TEXT   NOT NULL,
  size   BIGINT NOT NULL,
  PRIMARY KEY (run_id, path)
);
CREATE INDEX run_files_sha256 ON run_files(sha256);
