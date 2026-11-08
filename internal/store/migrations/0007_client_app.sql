-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- The application that started a run on its user's behalf (a token's azp,
-- appid or client_id), when the caller's token says; '' for basic auth.
ALTER TABLE runs ADD COLUMN client_app TEXT NOT NULL DEFAULT '';
