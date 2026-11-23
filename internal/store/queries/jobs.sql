-- name: EnqueueJob :execrows
-- EnqueueJob adds a ready job, unless a job of any state has the same
-- idempotency key, and returns how many rows it added: 1, or 0 for a
-- duplicate. A NULL key never conflicts.
INSERT INTO jobs (kind, payload, idempotency_key, state, run_at)
VALUES (?, ?, ?, 'ready', ?)
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: ClaimJob :one
-- ClaimJob leases the job due first, ready and due by now or leased until
-- now at the latest, until lease_until, and counts the attempt. The count
-- is the lease's generation: each claim raises it, so the updates below
-- match a lease by it. Run in an IMMEDIATE transaction, two claims never
-- take one job.
UPDATE jobs
SET state = 'leased', attempts = attempts + 1, lease_until = sqlc.arg(lease_until)
WHERE id = (
    SELECT due.id FROM jobs AS due
    WHERE (due.state = 'ready' AND due.run_at <= sqlc.arg(now))
       OR (due.state = 'leased' AND due.lease_until <= sqlc.arg(now))
    ORDER BY due.run_at, due.id
    LIMIT 1
)
RETURNING id, kind, payload, attempts;

-- name: CompleteJob :execrows
-- CompleteJob marks a job done, if the lease of generation attempts is
-- still its own; it returns 0 when another claim has taken it since.
UPDATE jobs SET state = 'done', lease_until = NULL
WHERE id = ? AND state = 'leased' AND attempts = ?;

-- name: RetryJob :execrows
-- RetryJob makes a job ready again at run_at, recording last_error, if the
-- lease of generation attempts is still its own.
UPDATE jobs SET state = 'ready', run_at = ?, lease_until = NULL, last_error = ?
WHERE id = ? AND state = 'leased' AND attempts = ?;

-- name: DeadLetterJob :execrows
-- DeadLetterJob moves a job to the dead letter, recording last_error, if
-- the lease of generation attempts is still its own. A dead job is never
-- claimed again.
UPDATE jobs SET state = 'dead', lease_until = NULL, last_error = ?
WHERE id = ? AND state = 'leased' AND attempts = ?;

-- The licence closes this file rather than opening it: sqlc copies the
-- comments above a query into the code it generates, so a new query goes
-- above these lines.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
