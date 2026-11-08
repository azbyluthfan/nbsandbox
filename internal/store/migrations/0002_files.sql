-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- The file index: which path in a space has which contents. The contents
-- themselves are in object storage under their SHA-256 (blob.ContentKey),
-- stored once however many paths, spaces or function versions use them.
-- Renames, moves and folder deletes are changes here, in one transaction.
CREATE TABLE files (
  space_id    TEXT   NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  path        TEXT   NOT NULL,                 -- relative, "/"-separated, validated by spaces
  kind        TEXT   NOT NULL,                 -- file | folder
  sha256      TEXT   NOT NULL DEFAULT '',      -- contents (files only)
  size        BIGINT NOT NULL DEFAULT 0,
  modified_at BIGINT NOT NULL,
  PRIMARY KEY (space_id, path),
  CHECK (kind IN ('file', 'folder')),
  CHECK (kind = 'folder' OR length(sha256) = 64)
);
-- Prefix searches for a folder's contents ("data/%"), independent of locale.
CREATE INDEX files_space_path ON files(space_id, path text_pattern_ops);
CREATE INDEX files_sha256 ON files(sha256) WHERE kind = 'file';

-- The files of each published function version: an immutable copy of
-- index rows (not bytes) taken at publish time.
CREATE TABLE function_version_files (
  space_id TEXT    NOT NULL,
  function TEXT    NOT NULL,
  version  INTEGER NOT NULL,
  path     TEXT    NOT NULL,
  sha256   TEXT    NOT NULL,
  size     BIGINT  NOT NULL,
  PRIMARY KEY (space_id, function, version, path),
  FOREIGN KEY (space_id, function, version) REFERENCES function_versions(space_id, function, version) ON DELETE CASCADE
);
CREATE INDEX function_version_files_sha256 ON function_version_files(sha256);
