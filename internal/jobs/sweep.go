// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs

import (
	"context"
	"database/sql"
	"time"

	"github.com/divbast/pisati/internal/store/storedb"
)

// sweepBatch bounds how many jobs one sweep transaction deletes, so that a
// sweep holds the write lock only briefly at a time, however many jobs it
// has to delete.
const sweepBatch = 500

// sweepEvery sweeps every Options.SweepEvery until ctx ends.
func (w *Worker) sweepEvery(ctx context.Context) {
	tick := time.NewTicker(w.opts.SweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.sweep(ctx)
		}
	}
}

// sweep deletes the jobs done for longer than Options.KeepDone and those
// dead for longer than Options.KeepDead, and with them their idempotency
// keys. It deletes a batch in each transaction, until one comes back
// short, so that other writers get the lock between batches.
func (w *Worker) sweep(ctx context.Context) {
	var removed int64
	for {
		var n int64
		err := w.store.InTx(ctx, func(q *storedb.Queries) error {
			now := w.clock()
			var err error
			n, err = q.SweepJobs(ctx, storedb.SweepJobsParams{
				DoneBefore: sql.NullInt64{Int64: now.Add(-w.opts.KeepDone).UnixMilli(), Valid: true},
				DeadBefore: sql.NullInt64{Int64: now.Add(-w.opts.KeepDead).UnixMilli(), Valid: true},
				Batch:      sweepBatch,
			})
			return err
		})
		if err != nil {
			// A sweep its context cut off was rolled back, and is no
			// failure.
			if ctx.Err() == nil {
				w.log.ErrorContext(ctx, "old jobs not all deleted", "removed", removed, "error", err)
			}
			return
		}
		removed += n
		if n < sweepBatch {
			break
		}
	}
	if removed > 0 {
		w.log.InfoContext(ctx, "old jobs deleted", "removed", removed)
	}
}
