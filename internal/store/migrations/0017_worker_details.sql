-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- The compute view (docs/plan-compute-view.md): what each worker is, and
-- what it can take.
ALTER TABLE workers ADD COLUMN role       TEXT   NOT NULL DEFAULT '';   -- worker, or all (one process)
ALTER TABLE workers ADD COLUMN version    TEXT   NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN started_at BIGINT NOT NULL DEFAULT 0;    -- unix ms
ALTER TABLE workers ADD COLUMN run_slots     INTEGER NOT NULL DEFAULT 0; -- runs.concurrency
ALTER TABLE workers ADD COLUMN session_slots INTEGER NOT NULL DEFAULT 0; -- sessions.per_worker (0: none)
