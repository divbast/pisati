// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// newStore returns a store at Pisati's schema, in a new state directory of
// its own, and closes it when the test ends.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	lock, err := store.AcquireLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("AcquireLock(%s): %v", dir, err)
	}
	s, err := store.Open(t.Context(), store.DatabasePath(dir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	_, _, err = s.Migrate(t.Context(), lock)
	if rerr := lock.Release(); rerr != nil {
		t.Fatal(rerr)
	}
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// inBubble runs f in a synctest bubble, with a store at Pisati's schema.
// The store is opened outside the bubble, so that the goroutine
// database/sql starts for it is the test's, not the bubble's.
func inBubble(t *testing.T, f func(t *testing.T, s *store.Store)) {
	t.Helper()
	s := newStore(t)
	synctest.Test(t, func(t *testing.T) {
		f(t, s)
	})
}

// kind returns name as a Kind, failing the test if it is not one.
func kind(t *testing.T, name string) jobs.Kind {
	t.Helper()
	k, err := jobs.NewKind(name)
	if err != nil {
		t.Fatalf("NewKind(%q): %v", name, err)
	}
	return k
}

// key returns s as an IdempotencyKey, failing the test if it is not one.
func key(t *testing.T, s string) jobs.IdempotencyKey {
	t.Helper()
	k, err := jobs.NewIdempotencyKey(s)
	if err != nil {
		t.Fatalf("NewIdempotencyKey(%q): %v", s, err)
	}
	return k
}

// enqueue commits j in a transaction of its own, and reports whether
// Enqueue added it.
func enqueue(t *testing.T, s *store.Store, j jobs.Job) bool {
	t.Helper()
	ctx := t.Context()
	var added bool
	if err := s.InTx(ctx, func(q *storedb.Queries) error {
		var err error
		added, err = jobs.Enqueue(ctx, q, j)
		return err
	}); err != nil {
		t.Fatalf("Enqueue(%s): %v", j.Kind, err)
	}
	return added
}

// jobRows returns every row of the jobs table, in the order they were
// added.
func jobRows(t *testing.T, s *store.Store) []storedb.Job {
	t.Helper()
	rows, err := s.DB().QueryContext(t.Context(), `SELECT id, kind, payload, idempotency_key, state,
		attempts, run_at, lease_until, lease_token, finished_at, last_error FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var all []storedb.Job
	for rows.Next() {
		var j storedb.Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.Payload, &j.IdempotencyKey, &j.State,
			&j.Attempts, &j.RunAt, &j.LeaseUntil, &j.LeaseToken, &j.FinishedAt, &j.LastError); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		all = append(all, j)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return all
}

// onlyRow returns the one row of the jobs table, failing the test unless
// there is exactly one.
func onlyRow(t *testing.T, s *store.Store) storedb.Job {
	t.Helper()
	all := jobRows(t, s)
	if len(all) != 1 {
		t.Fatalf("%d jobs, want 1: %+v", len(all), all)
	}
	return all[0]
}

// wantRow fails the test unless row is in state after attempts attempts,
// with lastError its last error ("" for none).
func wantRow(t *testing.T, row storedb.Job, state string, attempts int64, lastError string) {
	t.Helper()
	if row.State != state || row.Attempts != attempts {
		t.Errorf("job %d (%s): %s after %d attempts, want %s after %d", row.ID, row.Kind,
			row.State, row.Attempts, state, attempts)
	}
	if row.LastError.String != lastError {
		t.Errorf("job %d (%s): last error %q, want %q", row.ID, row.Kind, row.LastError.String, lastError)
	}
}

// logRecord is one record a recorder kept: its level, message and
// attributes, each value as fmt prints it, and each value's kind, which
// says how a JSON handler writes it.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
	kinds map[string]slog.Kind
}

// recorder is a slog.Handler that keeps every record, for a test to read.
type recorder struct {
	mu      sync.Mutex
	records []logRecord
}

// newLogger returns a logger that writes to a new recorder.
func newLogger() (*slog.Logger, *recorder) {
	r := &recorder{}
	return slog.New(r), r
}

func (*recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{}
	kinds := map[string]slog.Kind{}
	rec.Attrs(func(a slog.Attr) bool {
		v := a.Value.Resolve()
		attrs[a.Key] = v.String()
		kinds[a.Key] = v.Kind()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, logRecord{level: rec.Level, msg: rec.Message, attrs: attrs, kinds: kinds})
	return nil
}

// The worker logs through its logger as given, so the two below need
// do nothing more than satisfy slog.Handler.
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

// find returns the records with the message msg.
func (r *recorder) find(msg string) []logRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []logRecord
	for _, rec := range r.records {
		if rec.msg == msg {
			found = append(found, rec)
		}
	}
	return found
}

// atLeast returns the records at level or above.
func (r *recorder) atLeast(level slog.Level) []logRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []logRecord
	for _, rec := range r.records {
		if rec.level >= level {
			found = append(found, rec)
		}
	}
	return found
}

// mentions reports whether any record's message or attribute value
// contains text.
func (r *recorder) mentions(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.records {
		if strings.Contains(rec.msg, text) {
			return true
		}
		for _, v := range rec.attrs {
			if strings.Contains(v, text) {
				return true
			}
		}
	}
	return false
}

// fixedRand is a random source each 8 bytes of which are the same, so
// that every retry the worker schedules gets the same jitter.
type fixedRand [8]byte

func (f fixedRand) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = f[i%len(f)]
	}
	return len(p), nil
}

// The random sources the tests use, and the number in [0, 1) the worker
// reads from each: its top 53 bits over 2^53.
var (
	lowest  = fixedRand{}                                               // 0
	half    = fixedRand{0x80}                                           // 0.5: no jitter
	highest = fixedRand{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff} // 1 - 2^-53
)

// at returns a function that always returns u, as Backoff's rnd.
func at(u float64) func() float64 {
	return func() float64 { return u }
}

// failingRand is a random source that cannot be read.
type failingRand struct{}

func (failingRand) Read([]byte) (int, error) {
	return 0, errors.New("no randomness to be had")
}

// newWorker returns a Worker on s, reading the clock with time.Now (the
// bubble's fake clock inside one) and logging to the recorder it returns.
func newWorker(t *testing.T, s *store.Store, h map[jobs.Kind]jobs.Handler, opts jobs.Options, rnd io.Reader) (*jobs.Worker, *recorder) {
	t.Helper()
	log, rec := newLogger()
	w, err := jobs.NewWorker(s, h, time.Now, rnd, log, opts)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w, rec
}

// start runs w until the stop function it returns is called, or the test
// ends. stop cancels Run's context, waits for Run to return and returns
// its error; calling it again returns the same.
func start(t *testing.T, w *jobs.Worker) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()
	stop = sync.OnceValue(func() error {
		cancel()
		return <-done
	})
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return stop
}

// stopped calls stop, failing the test if Run returned an error.
func stopped(t *testing.T, stop func() error) {
	t.Helper()
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// noTriesLeft is the last error a job claimed with no tries left is
// dead-lettered with.
const noTriesLeft = "The last try did not finish: Pisati stopped while the job ran, or the try ran longer than its time limit."

// payloadOf returns the payload of a test's job number n.
func payloadOf(n int) []byte {
	return fmt.Appendf(nil, "job-%d", n)
}
