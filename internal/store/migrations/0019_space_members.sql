-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Shared spaces (docs/plan-shared-spaces.md): roles a space grants to other
-- callers. The space's creator (spaces.owner, its account) is an owner
-- without a row here.
CREATE TABLE space_members (
  space_id   TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  principal  TEXT NOT NULL,                -- a caller ID, as runs record it
  role       TEXT NOT NULL CHECK (role IN ('read', 'write', 'owner')),
  granted_by TEXT NOT NULL,
  granted_at BIGINT NOT NULL,
  PRIMARY KEY (space_id, principal)
);
CREATE INDEX space_members_principal ON space_members(principal);

-- Sessions are their opener's: one open session per notebook per caller
-- (two members of a space each get their own). Still one read-write
-- session per space.
DROP INDEX one_open_session_per_notebook;
CREATE UNIQUE INDEX one_open_session_per_notebook ON sessions(space_id, notebook, owner) WHERE status <> 'ended';
