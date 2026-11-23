-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

-- The schema Pisati starts from. Every table is STRICT, so that SQLite
-- refuses a value of the wrong type rather than storing it, and every
-- timestamp is an INTEGER count of milliseconds since the Unix epoch, UTC.
--
-- There is no Down section: Pisati never migrates a database down. A
-- database goes back to an earlier state by being restored from a backup.

-- +goose Up

-- One row a signed-in browser. Only the SHA-256 of the session ID is
-- kept, never the ID itself. user_id has no foreign key: the table of
-- users comes later.
CREATE TABLE sessions (
    id_hash BLOB PRIMARY KEY,
    user_id INTEGER,
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
) STRICT;

-- For the sweep, which deletes the sessions that have expired.
CREATE INDEX sessions_by_expiry ON sessions (expires_at);

-- For signing a user out everywhere, which deletes all of their sessions.
CREATE INDEX sessions_by_user ON sessions (user_id);

-- The persisted job queue. A job is ready, leased to a worker until
-- lease_until, done, or dead once it may be tried no more.
CREATE TABLE jobs (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL,
    payload BLOB NOT NULL,
    idempotency_key TEXT UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('ready', 'leased', 'done', 'dead')),
    attempts INTEGER NOT NULL DEFAULT 0,
    run_at INTEGER NOT NULL,
    lease_until INTEGER,
    last_error TEXT
) STRICT;

-- For a worker's claim, which takes the ready job that is due first.
CREATE INDEX jobs_by_state_run_at ON jobs (state, run_at);

-- The data keys, each wrapped by the master key. New data is sealed with
-- the current one; a retired one only opens what it sealed.
CREATE TABLE data_keys (
    id INTEGER PRIMARY KEY,
    wrapped BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('current', 'retired'))
) STRICT;
