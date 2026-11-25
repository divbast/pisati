// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs_test

import (
	"context"
	"database/sql"
	"log/slog"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// orphanedError is the last error ReleaseOrphans records with each job it
// releases.
const orphanedError = "An earlier try did not finish, because the Pisati process that ran it stopped."

// orphan claims the job due first as a Worker of a run that then stopped
// would leave it: leased until long after the test, its outcome never
// recorded. It returns the claim.
func orphan(t *testing.T, s *store.Store) storedb.ClaimJobRow {
	t.Helper()
	ctx := t.Context()
	var j storedb.ClaimJobRow
	if err := s.InTx(ctx, func(q *storedb.Queries) error {
		now := time.Now()
		var err error
		j, err = q.ClaimJob(ctx, storedb.ClaimJobParams{
			Now:        now.UnixMilli(),
			LeaseUntil: sql.NullInt64{Int64: now.Add(time.Hour).UnixMilli(), Valid: true},
		})
		return err
	}); err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	return j
}

// settled claims the job due first and records outcome for it, through q,
// as a Worker would have: done or dead.
func settled(t *testing.T, s *store.Store, outcome func(q *storedb.Queries, j storedb.ClaimJobRow) (int64, error)) {
	t.Helper()
	j := orphan(t, s)
	if err := s.InTx(t.Context(), func(q *storedb.Queries) error {
		_, err := outcome(q, j)
		return err
	}); err != nil {
		t.Fatalf("recording job %d's outcome: %v", j.ID, err)
	}
}

func TestReleaseOrphans(t *testing.T) {
	t.Run("leased jobs are ready at once, their tries counted", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			ctx := t.Context()
			k := kind(t, "test.orphan")

			// Two jobs a stopped run left leased, then one done, one
			// dead and one ready, which must stay as they are.
			enqueue(t, s, jobs.Job{Kind: k, Payload: payloadOf(1)})
			enqueue(t, s, jobs.Job{Kind: k, Payload: payloadOf(2)})
			orphan(t, s)
			orphan(t, s)
			now := sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}
			enqueue(t, s, jobs.Job{Kind: k, Payload: payloadOf(3)})
			settled(t, s, func(q *storedb.Queries, j storedb.ClaimJobRow) (int64, error) {
				return q.CompleteJob(ctx, storedb.CompleteJobParams{FinishedAt: now, ID: j.ID, LeaseToken: j.LeaseToken})
			})
			enqueue(t, s, jobs.Job{Kind: k, Payload: payloadOf(4)})
			settled(t, s, func(q *storedb.Queries, j storedb.ClaimJobRow) (int64, error) {
				return q.DeadLetterJob(ctx, storedb.DeadLetterJobParams{
					LastError: sql.NullString{String: "given up", Valid: true}, FinishedAt: now, ID: j.ID, LeaseToken: j.LeaseToken,
				})
			})
			enqueue(t, s, jobs.Job{Kind: k, Payload: payloadOf(5)})
			before := jobRows(t, s)

			var mu sync.Mutex
			ranAt := map[string]time.Time{}
			w, log := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: func(_ context.Context, payload []byte) error {
				mu.Lock()
				defer mu.Unlock()
				ranAt[string(payload)] = time.Now()
				return nil
			}}, oneWorker(), half)

			released, err := w.ReleaseOrphans(ctx)
			if err != nil {
				t.Fatalf("ReleaseOrphans: %v", err)
			}
			if released != 2 {
				t.Errorf("ReleaseOrphans released %d jobs, want 2", released)
			}
			after := jobRows(t, s)
			for i, row := range after {
				was := before[i]
				if i >= 2 {
					if !reflect.DeepEqual(row, was) {
						t.Errorf("job %d changed from %+v to %+v, want it left as it was", row.ID, was, row)
					}
					continue
				}
				wantRow(t, row, "ready", 1, orphanedError)
				if row.LeaseUntil.Valid {
					t.Errorf("job %d still has a lease, until %d", row.ID, row.LeaseUntil.Int64)
				}
				if row.RunAt != was.RunAt || row.LeaseToken != was.LeaseToken {
					t.Errorf("job %d: run_at %d and lease token %d, want them as they were, %d and %d",
						row.ID, row.RunAt, row.LeaseToken, was.RunAt, was.LeaseToken)
				}
			}
			logged := log.find("job made ready again: the Pisati process that was running it stopped")
			if len(logged) != 2 {
				t.Fatalf("logged %d released jobs, want 2", len(logged))
			}
			ids := map[string]bool{}
			for _, rec := range logged {
				ids[rec.attrs["job_id"]] = true
				if rec.attrs["kind"] != k.String() || rec.attrs["attempt"] != "1" {
					t.Errorf("released job %s logged as %v, want its kind and attempt 1", rec.attrs["job_id"], rec.attrs)
				}
			}
			for _, row := range after[:2] {
				if !ids[strconv.FormatInt(row.ID, 10)] {
					t.Errorf("job %d's release was not logged", row.ID)
				}
			}

			// A Worker runs them at once: it does not wait for the leases
			// the stopped run took.
			released0 := time.Now()
			stop := start(t, w)
			synctest.Wait()
			stopped(t, stop)
			for _, n := range []int{1, 2} {
				at, ok := ranAt[string(payloadOf(n))]
				switch {
				case !ok:
					t.Errorf("job %d never ran", n)
				case at.Sub(released0) != 0:
					t.Errorf("job %d ran %v after it was released, want at once", n, at.Sub(released0))
				}
			}
			for _, row := range jobRows(t, s)[:2] {
				wantRow(t, row, "done", 2, orphanedError)
			}
		})
	})

	t.Run("a job cut off on its last try is dead-lettered by the next claim", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			ctx := t.Context()
			k := kind(t, "test.crashes")
			var ran atomic.Int32
			opts := oneWorker()
			w, log := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: func(context.Context, []byte) error {
				ran.Add(1)
				return nil
			}}, opts, half)
			enqueue(t, s, jobs.Job{Kind: k})

			// Each run claims the job, then stops before it records an
			// outcome, as a job that crashes Pisati makes it.
			for try := 1; try <= opts.MaxAttempts; try++ {
				orphan(t, s)
				released, err := w.ReleaseOrphans(ctx)
				if err != nil {
					t.Fatalf("try %d: ReleaseOrphans: %v", try, err)
				}
				if released != 1 {
					t.Fatalf("try %d: ReleaseOrphans released %d jobs, want 1", try, released)
				}
				wantRow(t, onlyRow(t, s), "ready", int64(try), orphanedError)
			}

			stop := start(t, w)
			synctest.Wait()
			stopped(t, stop)
			if n := ran.Load(); n != 0 {
				t.Errorf("the job ran %d times after its last try, want none", n)
			}
			wantRow(t, onlyRow(t, s), "dead", int64(opts.MaxAttempts)+1, noTriesLeft)
			if dead := log.find("job given up: it will not be tried again"); len(dead) != 1 {
				t.Errorf("logged %d dead letters, want 1", len(dead))
			}
		})
	})

	t.Run("no job is leased", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			k := kind(t, "test.orphan")
			enqueue(t, s, jobs.Job{Kind: k})
			before := jobRows(t, s)
			w, log := newWorker(t, s, map[jobs.Kind]jobs.Handler{}, oneWorker(), half)
			released, err := w.ReleaseOrphans(t.Context())
			if err != nil || released != 0 {
				t.Errorf("ReleaseOrphans = %d, %v; want 0, nil", released, err)
			}
			if after := jobRows(t, s); !reflect.DeepEqual(after[0], before[0]) {
				t.Errorf("the ready job changed from %+v to %+v", before[0], after[0])
			}
			if n := len(log.atLeast(slog.LevelDebug)); n != 0 {
				t.Errorf("logged %d records, want none", n)
			}
		})
	})

	t.Run("a Worker NewWorker did not make", func(t *testing.T) {
		for name, w := range map[string]*jobs.Worker{"nil": nil, "zero": {}} {
			if _, err := w.ReleaseOrphans(t.Context()); err == nil {
				t.Errorf("%s Worker: ReleaseOrphans returned no error", name)
			}
		}
	})
}
