-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Connections: a user's data sources (docs/plan-data-access.md). The
-- credential is never here: only where a secret provider keeps it.
CREATE TABLE connections (
  id              TEXT PRIMARY KEY,
  owner           TEXT NOT NULL,
  name            TEXT NOT NULL,
  type            TEXT NOT NULL,
  settings        TEXT NOT NULL DEFAULT '{}', -- JSON: host, port, database, user, bucket...; never a secret
  secret_mode     TEXT NOT NULL CHECK (secret_mode IN ('none', 'managed', 'referenced')),
  secret_provider TEXT NOT NULL DEFAULT '',
  secret_ref      TEXT NOT NULL DEFAULT '',   -- where the provider keeps it
  created_at      BIGINT NOT NULL,
  updated_at      BIGINT NOT NULL,
  CONSTRAINT connections_owner_name UNIQUE (owner, name)
);

-- The built-in secret store (development): values encrypted with the
-- operator's master key.
CREATE TABLE builtin_secrets (
  id         TEXT PRIMARY KEY,
  nonce      BYTEA NOT NULL,
  ciphertext BYTEA NOT NULL,
  created_at BIGINT NOT NULL
);

-- A space's environments (dev, prod...): named sets of bindings.
CREATE TABLE space_environments (
  space_id   TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  is_default BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY (space_id, name)
);
CREATE UNIQUE INDEX one_default_environment ON space_environments(space_id) WHERE is_default;

-- What a name means in a space's environment. A bound connection can't be
-- deleted (RESTRICT): it must be unbound first.
CREATE TABLE space_bindings (
  space_id      TEXT NOT NULL,
  environment   TEXT NOT NULL,
  name          TEXT NOT NULL,
  connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE RESTRICT,
  PRIMARY KEY (space_id, environment, name),
  FOREIGN KEY (space_id, environment) REFERENCES space_environments(space_id, name) ON DELETE CASCADE
);
CREATE INDEX space_bindings_connection ON space_bindings(connection_id);
