// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/divbast/pisati/internal/store/storedb"
)

// orphanedError is the last error kept with each job ReleaseOrphans
// releases, saying why its try counted though it never finished.
const orphanedError = "the previous run of Pisati stopped while this attempt was running"

// ReleaseOrphans makes ready again, at once, every job still leased in
// the Worker's database: the jobs a run of Pisati had claimed when it
// stopped without recording their outcome, as a crash, a kill or a power
// cut stops it. Each keeps its due time, so it is due at once, and its
// attempts, so the try that was cut off counts. A job whose last try was
// cut off is therefore claimed once more, found past Options.MaxAttempts,
// and moved to the dead letter without running: a job that stops Pisati
// each time it runs reaches the dead letter after Options.MaxAttempts
// tries. ReleaseOrphans dead-letters nothing itself. It records why with
// each job it releases, logs each one, and returns how many it released.
//
// It must run only while no Worker anywhere runs on the database: under
// the state directory's lock (store.AcquireLock), which serve holds for
// as long as it runs, and before Run. A lease a running Worker holds would
// otherwise be taken from it, and its job run twice at once.
//
// It returns an error, changing nothing, for a Worker NewWorker did not
// make, or when the release cannot be written.
func (w *Worker) ReleaseOrphans(ctx context.Context) (int, error) {
	if w == nil || w.store == nil {
		return 0, errors.New("jobs: release orphaned jobs: the Worker was not made by NewWorker")
	}
	var released []storedb.ReleaseOrphanedJobsRow
	if err := w.store.InTx(ctx, func(q *storedb.Queries) error {
		var err error
		released, err = q.ReleaseOrphanedJobs(ctx, lastError(orphanedError))
		return err
	}); err != nil {
		return 0, fmt.Errorf("jobs: release orphaned jobs: %w", err)
	}
	for _, j := range released {
		w.log.InfoContext(ctx, "job released: the previous run stopped during it",
			"kind", j.Kind, "job_id", j.ID, "attempt", j.Attempts)
	}
	return len(released), nil
}
