-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Besides status changes, a run's events carry what it reports while it
-- works: partial results, progress and cell starts and ends. They share the
-- sequence, the streams, resuming and retention. `status` is the run's
-- status when the event was recorded.
ALTER TABLE run_events ADD COLUMN kind TEXT NOT NULL DEFAULT 'status'; -- status | item | progress | cell
