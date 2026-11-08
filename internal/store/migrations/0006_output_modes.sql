-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- How a run returns its result: one value (single) or a sequence of items
-- (stream), from its contract or inferred from the notebook's calls. A
-- function version keeps the mode it was published with, and whether the
-- contract declared it.
ALTER TABLE runs ADD COLUMN output_mode TEXT NOT NULL DEFAULT 'single';
ALTER TABLE function_versions ADD COLUMN contract_mode TEXT NOT NULL DEFAULT 'single';
ALTER TABLE function_versions ADD COLUMN contract_mode_declared BOOLEAN NOT NULL DEFAULT false;
