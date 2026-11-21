-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Where API nodes reach a worker's interactive sessions (host:port of its
-- internal listener; '' when it holds none).
ALTER TABLE workers ADD COLUMN session_addr TEXT NOT NULL DEFAULT '';
