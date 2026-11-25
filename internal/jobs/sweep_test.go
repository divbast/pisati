// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store"
)

// sweepTest is a Worker whose clock runs ahead of the bubble's by as much
// as the test says, so that its jobs age days in the time between two
// sweeps. Its handlers finish test.ok jobs and send test.bad jobs to the
// dead letter.
type sweepTest struct {
	w     *jobs.Worker
	log   *recorder
	opts  jobs.Options
	ahead atomic.Int64
	ok    jobs.Kind
	bad   jobs.Kind
}

func newSweepTest(t *testing.T, s *store.Store) *sweepTest {
	t.Helper()
	st := &sweepTest{opts: oneWorker(), ok: kind(t, "test.ok"), bad: kind(t, "test.bad")}
	st.opts.Poll = time.Minute
	log, rec := newLogger()
	w, err := jobs.NewWorker(s, map[jobs.Kind]jobs.Handler{
		st.ok:  func(context.Context, []byte) error { return nil },
		st.bad: func(context.Context, []byte) error { return &jobs.Permanent{} },
	}, func() time.Time {
		return time.Now().Add(time.Duration(st.ahead.Load()))
	}, half, log, st.opts)
	if err != nil {
		t.Fatal(err)
	}
	st.w, st.log = w, rec
	return st
}

// states returns the state of each job, in the order they were added.
func states(t *testing.T, s *store.Store) []string {
	t.Helper()
	var got []string
	for _, row := range jobRows(t, s) {
		got = append(got, row.State)
	}
	return got
}

// wantStates fails the test unless the jobs are in the states want.
func wantStates(t *testing.T, s *store.Store, when string, want ...string) {
	t.Helper()
	if got := states(t, s); !slices.Equal(got, want) {
		t.Errorf("%s: jobs %q, want %q", when, got, want)
	}
}

func TestSweepRemovesOldJobs(t *testing.T) {
	t.Run("every 10 minutes, once their retention has passed", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			st := newSweepTest(t, s)
			doneKey, deadKey := key(t, "done"), key(t, "dead")
			nextYear := time.Now().AddDate(1, 0, 0)
			enqueue(t, s, jobs.Job{Kind: st.ok, Key: doneKey})
			enqueue(t, s, jobs.Job{Kind: st.bad, Key: deadKey})
			enqueue(t, s, jobs.Job{Kind: st.ok, RunAt: nextYear})

			stop := start(t, st.w)
			synctest.Wait()
			wantStates(t, s, "once run", "done", "dead", "ready")

			// The sweep 10 minutes in finds the done job two hours short of
			// its retention.
			st.ahead.Store(int64(st.opts.KeepDone - 2*time.Hour))
			time.Sleep(st.opts.SweepEvery + time.Minute)
			wantStates(t, s, "short of a week", "done", "dead", "ready")

			// Past it now, but no sweep runs until the next one, 10 minutes on.
			st.ahead.Store(int64(st.opts.KeepDone))
			time.Sleep(st.opts.SweepEvery - 2*time.Minute)
			wantStates(t, s, "between sweeps", "done", "dead", "ready")
			time.Sleep(2 * time.Minute)
			wantStates(t, s, "a week on", "dead", "ready")
			if !enqueue(t, s, jobs.Job{Kind: st.ok, Key: doneKey, RunAt: nextYear}) {
				t.Error("the key of a done job swept is still taken")
			}

			st.ahead.Store(int64(st.opts.KeepDead))
			time.Sleep(st.opts.SweepEvery)
			wantStates(t, s, "thirty days on", "ready", "ready")
			if !enqueue(t, s, jobs.Job{Kind: st.ok, Key: deadKey, RunAt: nextYear}) {
				t.Error("the key of a dead job swept is still taken")
			}
			stopped(t, stop)
			if swept := st.log.find("old jobs deleted"); len(swept) != 2 {
				t.Errorf("logged %d sweeps that removed jobs, want 2: %+v", len(swept), swept)
			}
		})
	})

	t.Run("as Run starts", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			st := newSweepTest(t, s)
			enqueue(t, s, jobs.Job{Kind: st.ok})
			enqueue(t, s, jobs.Job{Kind: st.bad})
			stop := start(t, st.w)
			synctest.Wait()
			stopped(t, stop)
			wantStates(t, s, "once run", "done", "dead")

			st.ahead.Store(int64(st.opts.KeepDead))
			stop = start(t, st.w)
			synctest.Wait()
			wantStates(t, s, "as Run starts again, thirty days on")
			stopped(t, stop)
		})
	})

	t.Run("in batches", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			st := newSweepTest(t, s)
			// More done jobs, long past their retention, than one batch
			// takes; and one just finished.
			const old = 1201
			if _, err := s.DB().ExecContext(t.Context(), `
				WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
				INSERT INTO jobs (kind, payload, state, attempts, run_at, lease_token, finished_at)
				SELECT 'test.ok', x'', 'done', 1, 0, 1, 0 FROM n`, old); err != nil {
				t.Fatal(err)
			}
			enqueue(t, s, jobs.Job{Kind: st.ok})

			stop := start(t, st.w)
			synctest.Wait()
			stopped(t, stop)
			wantStates(t, s, "after one sweep", "done")
			swept := st.log.find("old jobs deleted")
			if len(swept) != 1 || swept[0].attrs["removed"] != "1201" {
				t.Errorf("logged %+v, want one sweep that removed %d", swept, old)
			}
		})
	})
}
