-- name: InsertSession :exec
INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, expires_at)
VALUES (?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT id_hash, user_id, created_at, last_seen_at, expires_at
FROM sessions
WHERE id_hash = ?;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?;

-- The licence closes this file rather than opening it: sqlc copies the
-- comments above a query into the code it generates, so a new query goes
-- above these lines.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
