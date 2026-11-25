-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- What a published version reads (docs/plan-data-access.md): the names its
-- space bound when it was published, and their connection types, as JSON
-- ({"crm": "postgresql"}). An invocation resolves them with the caller's
-- own bindings, which must match.
ALTER TABLE function_versions ADD COLUMN connections TEXT NOT NULL DEFAULT '{}';
