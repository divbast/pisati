-- name: InsertSession :exec
INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, expires_at)
VALUES (?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT id_hash, user_id, created_at, last_seen_at, expires_at
FROM sessions
WHERE id_hash = ?;

-- name: TouchSession :execrows
-- TouchSession sets a session's last_seen_at and expires_at, unless it was
-- last seen after touch_before: of two requests that find a touch due at
-- once, only the first writes. It returns how many rows it changed.
UPDATE sessions
SET last_seen_at = sqlc.arg(last_seen_at), expires_at = sqlc.arg(expires_at)
WHERE id_hash = sqlc.arg(id_hash) AND last_seen_at <= sqlc.arg(touch_before);

-- name: DeleteSession :exec
-- DeleteSession deletes the session with the ID hash id_hash, if there is
-- one.
DELETE FROM sessions WHERE id_hash = ?;

-- name: DeleteUserSessions :execrows
-- DeleteUserSessions deletes every session of user_id, and returns how
-- many it deleted.
DELETE FROM sessions WHERE user_id = ?;

-- name: SweepSessions :execrows
-- SweepSessions deletes at most batch of the sessions that expire at now
-- or before, and returns how many it deleted.
DELETE FROM sessions
WHERE id_hash IN (
    SELECT old.id_hash FROM sessions AS old
    WHERE old.expires_at <= sqlc.arg(now)
    LIMIT sqlc.arg(batch)
);

-- The licence closes this file rather than opening it: sqlc copies the
-- comments above a query into the code it generates, so a new query goes
-- above these lines.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
