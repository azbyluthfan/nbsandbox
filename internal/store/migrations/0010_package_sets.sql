-- Copyright 2026 The nbsandbox Authors
-- SPDX-License-Identifier: Apache-2.0

-- Package sets: a space's extra packages, installed by a build container
-- once per (requirements, runner image, platform) and shared by every space
-- that asks for the same. The files are in object storage
-- (packages/<key>.tar). A set is pending until a worker claims its build.
CREATE TABLE package_sets (
	key          TEXT PRIMARY KEY,  -- packages.Key: what to install, runner image, platform
	status       TEXT NOT NULL,     -- pending, building, ready or failed
	requirements TEXT NOT NULL,     -- what to install (canonical requirements, or a lock)
	prefer_lock  TEXT NOT NULL,     -- a lock to try first (the space's last), '' if none
	from_lock    BOOLEAN NOT NULL,  -- requirements is a lock: install exactly it
	lock         TEXT NOT NULL,     -- what was installed: name==version --hash=..., one per line
	size_bytes   BIGINT NOT NULL,   -- unpacked
	log          TEXT NOT NULL,     -- the build's output (its end)
	error        TEXT NOT NULL,     -- why it failed, for the caller
	builder      TEXT NOT NULL,     -- the worker building it
	build_until  BIGINT NOT NULL,   -- unix ms: the build's lease; another worker takes over after it
	created_at   BIGINT NOT NULL,
	built_at     BIGINT NOT NULL,   -- 0 until it finishes
	last_used    BIGINT NOT NULL
);
CREATE INDEX package_sets_status ON package_sets (status, created_at);
CREATE INDEX package_sets_last_used ON package_sets (last_used);

-- A run that needs a package set waits, queued, until the set is ready:
-- workers claim only runs whose set is ready (or that need none).
ALTER TABLE runs ADD COLUMN package_key TEXT NOT NULL DEFAULT '';

-- A space's last built requirements and lock: a rebuild of the same
-- requirements tries the lock first, so versions don't drift.
ALTER TABLE spaces ADD COLUMN package_requirements TEXT NOT NULL DEFAULT '';
ALTER TABLE spaces ADD COLUMN package_lock TEXT NOT NULL DEFAULT '';

-- A published version keeps the exact packages it was published with: its
-- set's lock (empty: none). Invocations use the set built from it.
ALTER TABLE function_versions ADD COLUMN package_lock TEXT NOT NULL DEFAULT '';
