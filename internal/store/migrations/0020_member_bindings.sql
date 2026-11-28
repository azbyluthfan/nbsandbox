-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Names per member (docs/plan-shared-spaces.md): a space's owners declare
-- its names and their types; each member binds their own connections.
-- Today's bindings become the declarations (a name's type is its
-- connection's, in the default environment first) and the creator's own
-- bindings.
CREATE TABLE space_names (
  space_id TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  name     TEXT NOT NULL,
  type     TEXT NOT NULL,
  PRIMARY KEY (space_id, name)
);
INSERT INTO space_names (space_id, name, type)
  SELECT DISTINCT ON (b.space_id, b.name) b.space_id, b.name, c.type
  FROM space_bindings b
  JOIN connections c ON c.id = b.connection_id
  JOIN space_environments e ON e.space_id = b.space_id AND e.name = b.environment
  ORDER BY b.space_id, b.name, e.is_default DESC, b.environment;

ALTER TABLE space_bindings ADD COLUMN principal TEXT NOT NULL DEFAULT '';
UPDATE space_bindings b SET principal = s.owner FROM spaces s WHERE s.id = b.space_id;
ALTER TABLE space_bindings ALTER COLUMN principal DROP DEFAULT;
ALTER TABLE space_bindings DROP CONSTRAINT space_bindings_pkey;
ALTER TABLE space_bindings ADD PRIMARY KEY (space_id, principal, environment, name);
ALTER TABLE space_bindings ADD CONSTRAINT space_bindings_declared
  FOREIGN KEY (space_id, name) REFERENCES space_names(space_id, name) ON DELETE CASCADE;
