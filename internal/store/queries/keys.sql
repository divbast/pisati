-- name: ListDataKeys :many
-- ListDataKeys returns every data key, wrapped, in the order of their IDs.
SELECT id, wrapped, state FROM data_keys ORDER BY id;

-- name: NextDataKeyID :one
-- NextDataKeyID returns the ID one past the largest a data key has, or 1
-- when there is none. No data key is ever deleted, so no ID is used
-- twice. Run in an IMMEDIATE transaction, two callers never get the same
-- ID.
SELECT CAST(COALESCE(MAX(id), 0) + 1 AS INTEGER) AS next_id FROM data_keys;

-- name: InsertCurrentDataKey :exec
-- InsertCurrentDataKey stores the data key id, wrapped, as the current
-- one. The caller retires the key that was current first, in the same
-- transaction.
INSERT INTO data_keys (id, wrapped, created_at, state)
VALUES (?, ?, ?, 'current');

-- name: RetireDataKey :execrows
-- RetireDataKey makes the data key id retired, if it is the current one,
-- and returns how many rows it changed: 1, or 0 when id is not current.
UPDATE data_keys SET state = 'retired' WHERE id = ? AND state = 'current';

-- name: RewrapDataKey :execrows
-- RewrapDataKey replaces the wrapped form of the data key id, and returns
-- how many rows it changed: 1, or 0 when there is no such key.
UPDATE data_keys SET wrapped = ? WHERE id = ?;

-- The licence closes this file rather than opening it: sqlc copies the
-- comments above a query into the code it generates, so a new query goes
-- above these lines.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
