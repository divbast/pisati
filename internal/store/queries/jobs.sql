-- name: EnqueueJob :execrows
-- EnqueueJob adds a ready job, unless a job of any state has the same
-- idempotency key, and returns how many rows it added: 1, or 0 for a
-- duplicate. A NULL key never conflicts.
INSERT INTO jobs (kind, payload, idempotency_key, state, run_at)
VALUES (?, ?, ?, 'ready', ?)
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: ClaimJob :one
-- ClaimJob leases the job due first, ready and due by now or leased until
-- now at the latest, until lease_until. It counts the attempt and raises
-- the lease token, which no write lowers: the updates below match a lease
-- by its token. Run in an IMMEDIATE transaction, two claims never take
-- one job.
UPDATE jobs
SET state = 'leased', attempts = attempts + 1, lease_token = lease_token + 1,
    lease_until = sqlc.arg(lease_until)
WHERE id = (
    SELECT due.id FROM jobs AS due
    WHERE (due.state = 'ready' AND due.run_at <= sqlc.arg(now))
       OR (due.state = 'leased' AND due.lease_until <= sqlc.arg(now))
    ORDER BY due.run_at, due.id
    LIMIT 1
)
RETURNING id, kind, payload, attempts, lease_token, lease_until;

-- name: CompleteJob :execrows
-- CompleteJob marks a job done at finished_at, if the lease of token
-- lease_token is still its own; it returns 0 when another claim has
-- taken the job since.
UPDATE jobs SET state = 'done', lease_until = NULL, finished_at = ?
WHERE id = ? AND state = 'leased' AND lease_token = ?;

-- name: RetryJob :execrows
-- RetryJob makes a job ready again at run_at, recording last_error, if
-- the lease of token lease_token is still its own.
UPDATE jobs SET state = 'ready', run_at = ?, lease_until = NULL, last_error = ?
WHERE id = ? AND state = 'leased' AND lease_token = ?;

-- name: ReleaseJob :execrows
-- ReleaseJob gives a job back, if the lease of token lease_token is still
-- its own: ready again, as due as it was, and with the attempt its claim
-- counted taken back. The token stays as it is.
UPDATE jobs SET state = 'ready', attempts = attempts - 1, lease_until = NULL
WHERE id = ? AND state = 'leased' AND lease_token = ?;

-- name: DeadLetterJob :execrows
-- DeadLetterJob moves a job to the dead letter at finished_at, recording
-- last_error, if the lease of token lease_token is still its own. A dead
-- job is never claimed again.
UPDATE jobs SET state = 'dead', lease_until = NULL, last_error = ?, finished_at = ?
WHERE id = ? AND state = 'leased' AND lease_token = ?;

-- name: SweepJobs :execrows
-- SweepJobs deletes at most batch of the jobs done since before
-- done_before or dead since before dead_before, and returns how many it
-- deleted. Their idempotency keys go with them.
DELETE FROM jobs
WHERE id IN (
    SELECT old.id FROM jobs AS old
    WHERE (old.state = 'done' AND old.finished_at <= sqlc.arg(done_before))
       OR (old.state = 'dead' AND old.finished_at <= sqlc.arg(dead_before))
    LIMIT sqlc.arg(batch)
);

-- name: ReleaseOrphanedJobs :many
-- ReleaseOrphanedJobs makes every leased job ready again, as due as it
-- was, recording last_error, and returns each one's ID, kind and
-- attempts. It leaves attempts as they are, so that the try each lease
-- was claimed for counts, and the lease token as it is. It is run only
-- while no worker runs: every lease is then one a run that stopped left.
UPDATE jobs SET state = 'ready', lease_until = NULL, last_error = ?
WHERE state = 'leased'
RETURNING id, kind, attempts;

-- The licence closes this file rather than opening it: sqlc copies the
-- comments above a query into the code it generates, so a new query goes
-- above these lines.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
