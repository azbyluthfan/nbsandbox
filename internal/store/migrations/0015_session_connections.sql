-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Interactive sessions read connections as runs do (0014): the environment
-- a session read its space's names in ('' if none), and which connection
-- each name meant when it was opened.
ALTER TABLE sessions ADD COLUMN environment TEXT NOT NULL DEFAULT '';

CREATE TABLE session_connections (
  session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  connection_id TEXT NOT NULL,
  PRIMARY KEY (session_id, name)
);
