// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// TestClaimIsExclusive runs eight workers on one database while jobs are
// enqueued, each in a transaction of its own, from other goroutines: no
// job may run twice, and no worker may meet SQLITE_BUSY. It runs on real
// goroutines, not in a synctest bubble: what is under test is SQLite's
// file locking, which waits in system calls a bubble cannot see.
func TestClaimIsExclusive(t *testing.T) {
	s := newStore(t)
	k := kind(t, "test.exclusive")
	const workers, total, enqueuers = 8, 200, 4

	var mu sync.Mutex
	runs := map[string]int{}
	allRan := make(chan struct{})
	handlers := map[jobs.Kind]jobs.Handler{k: func(_ context.Context, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		runs[string(payload)]++
		if len(runs) == total && runs[string(payload)] == 1 {
			close(allRan)
		}
		return nil
	}}

	opts := jobs.DefaultOptions()
	opts.Workers = 1 // eight Workers of one each: eight claimers apart
	opts.Poll = 5 * time.Millisecond
	ws := make([]*jobs.Worker, workers)
	logs := make([]*recorder, workers)
	for i := range ws {
		ws[i], logs[i] = newWorker(t, s, handlers, opts, half)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var running sync.WaitGroup
	errs := make(chan error, workers)
	for _, w := range ws {
		running.Go(func() { errs <- w.Run(ctx) })
	}

	var enqueuing sync.WaitGroup
	for g := range enqueuers {
		enqueuing.Go(func() {
			for n := g; n < total; n += enqueuers {
				err := s.InTx(ctx, func(q *storedb.Queries) error {
					_, err := jobs.Enqueue(ctx, q, jobs.Job{Kind: k, Payload: payloadOf(n)})
					return err
				})
				if err != nil {
					t.Errorf("Enqueue job %d: %v", n, err)
					return
				}
				for _, w := range ws {
					w.Notify()
				}
			}
		})
	}
	enqueuing.Wait()

	select {
	case <-allRan:
	case <-time.After(time.Minute):
		t.Error("the jobs had not all run after a minute")
	}
	cancel()
	running.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(runs) != total {
		t.Errorf("%d jobs ran, want %d", len(runs), total)
	}
	for payload, n := range runs {
		if n != 1 {
			t.Errorf("%s ran %d times, want once", payload, n)
		}
	}
	all := jobRows(t, s)
	if len(all) != total {
		t.Errorf("%d jobs stored, want %d", len(all), total)
	}
	for _, row := range all {
		wantRow(t, row, "done", 1, "")
	}
	for i, log := range logs {
		for _, rec := range log.atLeast(slog.LevelWarn) {
			t.Errorf("worker %d logged %s %q %v; want nothing at warning or above", i, rec.level, rec.msg, rec.attrs)
		}
	}
}

func TestRetryBacksOffThenDeadLetters(t *testing.T) {
	inBubble(t, func(t *testing.T, s *store.Store) {
		k := kind(t, "test.flaky")
		errDown := errors.New("the far end is down")
		tries := make(chan time.Time, 100)
		handlers := map[jobs.Kind]jobs.Handler{k: func(context.Context, []byte) error {
			tries <- time.Now()
			return errDown
		}}
		// The approved backoff, with one worker polling every 30 s rather
		// than every second. Under the race detector an empty claim costs
		// about 0.6 ms, and the 16 h that twelve attempts take would need
		// 57,600 of them a worker. With half as its random source, every
		// wait is a multiple of 30 s, so each attempt still comes the
		// moment it is due.
		opts := jobs.DefaultOptions()
		opts.Workers = 1
		opts.Poll = opts.BackoffBase
		w, log := newWorker(t, s, handlers, opts, half)
		enqueue(t, s, jobs.Job{Kind: k, Payload: []byte("flaky")})

		stop := start(t, w)
		time.Sleep(24 * time.Hour)
		stopped(t, stop)
		close(tries)

		var times []time.Time
		for when := range tries {
			times = append(times, when)
		}
		if len(times) != opts.MaxAttempts {
			t.Fatalf("%d attempts, want %d", len(times), opts.MaxAttempts)
		}
		// Each retry comes its backoff after the attempt before it.
		for n := 1; n < len(times); n++ {
			if gap, want := times[n].Sub(times[n-1]), jobs.Backoff(n, at(0.5)); gap != want {
				t.Errorf("attempt %d came %v after attempt %d, want %v", n+1, gap, n, want)
			}
		}
		wantRow(t, onlyRow(t, s), "dead", int64(opts.MaxAttempts), errDown.Error())
		if got := log.find("job retry scheduled"); len(got) != opts.MaxAttempts-1 {
			t.Errorf("%d retries logged, want %d", len(got), opts.MaxAttempts-1)
		}
		dead := log.find("job dead-lettered")
		if len(dead) != 1 || dead[0].level != slog.LevelError || dead[0].attrs["kind"] != k.String() {
			t.Errorf("logged %+v, want one error naming the kind", dead)
		}
	})
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	// stuck is a handler that ignores its context, as a call that hangs
	// would. It closes entered once it runs, and returns once release is
	// closed.
	stuck := func(entered, release chan struct{}) jobs.Handler {
		return func(context.Context, []byte) error {
			close(entered)
			<-release
			return nil
		}
	}

	t.Run("by another worker", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			k := kind(t, "test.hangs")
			entered, release := make(chan struct{}), make(chan struct{})
			reentered, rerelease := make(chan struct{}), make(chan struct{})
			opts := jobs.DefaultOptions()
			opts.Workers = 1
			first, firstLog := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: stuck(entered, release)}, opts, half)
			second, _ := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: stuck(reentered, rerelease)}, opts, half)
			enqueue(t, s, jobs.Job{Kind: k})

			stopFirst := start(t, first)
			<-entered
			claimed := time.Now()
			stopSecond := start(t, second)
			<-reentered
			if took := time.Since(claimed); took != opts.Lease {
				t.Errorf("the job was claimed again %v after the first claim, want %v, once the lease expired", took, opts.Lease)
			}

			// The first worker's handler returns at last, while the
			// second holds the lease: its outcome must not be recorded.
			close(release)
			synctest.Wait()
			row := onlyRow(t, s)
			wantRow(t, row, "leased", 2, "")
			if lost := firstLog.find("job lease lost"); len(lost) != 1 {
				t.Errorf("the first worker logged %d lost leases, want 1", len(lost))
			}

			close(rerelease)
			synctest.Wait()
			wantRow(t, onlyRow(t, s), "done", 2, "")
			stopped(t, stopFirst)
			stopped(t, stopSecond)
		})
	})

	t.Run("on its last attempt", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			k := kind(t, "test.hangs")
			entered, release := make(chan struct{}), make(chan struct{})
			var reran atomic.Int32
			opts := jobs.DefaultOptions()
			opts.Workers = 1
			opts.MaxAttempts = 1
			first, _ := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: stuck(entered, release)}, opts, half)
			second, secondLog := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: func(context.Context, []byte) error {
				reran.Add(1)
				return nil
			}}, opts, half)
			enqueue(t, s, jobs.Job{Kind: k})

			stopFirst := start(t, first)
			<-entered
			stopSecond := start(t, second)
			time.Sleep(opts.Lease + time.Second)
			if reran.Load() != 0 {
				t.Errorf("the job ran again after its last attempt")
			}
			row := onlyRow(t, s)
			wantRow(t, row, "dead", 2, "no attempts left: the last attempt's lease expired")
			if dead := secondLog.find("job dead-lettered"); len(dead) != 1 {
				t.Errorf("logged %d dead letters, want 1", len(dead))
			}
			close(release)
			stopped(t, stopFirst)
			stopped(t, stopSecond)
		})
	})
}

func TestUnknownKindDeadLetters(t *testing.T) {
	inBubble(t, func(t *testing.T, s *store.Store) {
		known, unknown := kind(t, "test.known"), kind(t, "test.unknown")
		var ran atomic.Int32
		w, log := newWorker(t, s, map[jobs.Kind]jobs.Handler{known: func(context.Context, []byte) error {
			ran.Add(1)
			return nil
		}}, jobs.DefaultOptions(), half)
		enqueue(t, s, jobs.Job{Kind: unknown, Payload: []byte("private-payload")})
		enqueue(t, s, jobs.Job{Kind: known})

		stop := start(t, w)
		time.Sleep(time.Second)
		stopped(t, stop)

		all := jobRows(t, s)
		if len(all) != 2 {
			t.Fatalf("%d jobs, want 2", len(all))
		}
		wantRow(t, all[0], "dead", 1, "no handler for job kind test.unknown")
		wantRow(t, all[1], "done", 1, "")
		if ran.Load() != 1 {
			t.Errorf("the known kind's handler ran %d times, want once", ran.Load())
		}
		got := log.find("job kind has no handler")
		if len(got) != 1 || got[0].level != slog.LevelError || got[0].attrs["kind"] != unknown.String() {
			t.Errorf("logged %+v, want one error naming the kind", got)
		}
		if log.mentions("private-payload") {
			t.Error("the payload was logged")
		}
	})
}

func TestHandlerPanicDeadLetters(t *testing.T) {
	inBubble(t, func(t *testing.T, s *store.Store) {
		panics, fine := kind(t, "test.panics"), kind(t, "test.fine")
		var ran atomic.Int32
		w, log := newWorker(t, s, map[jobs.Kind]jobs.Handler{
			panics: func(_ context.Context, payload []byte) error {
				panic("cannot handle " + string(payload))
			},
			fine: func(context.Context, []byte) error {
				ran.Add(1)
				return nil
			},
		}, jobs.DefaultOptions(), half)
		enqueue(t, s, jobs.Job{Kind: panics, Payload: []byte("private-payload")})
		enqueue(t, s, jobs.Job{Kind: fine})

		stop := start(t, w)
		time.Sleep(time.Second)
		stopped(t, stop)

		all := jobRows(t, s)
		if len(all) != 2 {
			t.Fatalf("%d jobs, want 2", len(all))
		}
		wantRow(t, all[0], "dead", 1, "handler panicked")
		wantRow(t, all[1], "done", 1, "")
		if ran.Load() != 1 {
			t.Errorf("after the panic, the next job ran %d times, want once", ran.Load())
		}
		got := log.find("job handler panicked")
		if len(got) != 1 || got[0].level != slog.LevelError {
			t.Fatalf("logged %+v, want one error", got)
		}
		if got[0].attrs["kind"] != panics.String() || got[0].attrs["job_id"] != strconv.FormatInt(all[0].ID, 10) {
			t.Errorf("the panic was logged with %v, want its kind and job ID", got[0].attrs)
		}
		if log.mentions("private-payload") {
			t.Error("the payload was logged")
		}
	})
}

func TestHandlerOutcomes(t *testing.T) {
	errCause := errors.New("cause")
	for _, c := range []struct {
		name      string
		err       error
		state     string
		after     time.Duration // when state is ready: how long after the attempt the retry is due
		lastError string
	}{
		{"nil", nil, "done", 0, ""},
		{"a bare error", errCause, "ready", base, "cause"},
		{"a Retry with After", &jobs.Retry{After: 10 * time.Minute, Err: errCause}, "ready", 10 * time.Minute, "cause"},
		{"a Retry without After", &jobs.Retry{Err: errCause}, "ready", base, "cause"},
		{"a Retry with a negative After", &jobs.Retry{After: -time.Minute, Err: errCause}, "ready", base, "cause"},
		{"a Retry past the cap", &jobs.Retry{After: 48 * time.Hour, Err: errCause}, "ready", backCap, "cause"},
		{"a wrapped Retry", fmt.Errorf("fetch: %w", &jobs.Retry{After: time.Minute, Err: errCause}), "ready", time.Minute, "fetch: cause"},
		{"a Permanent", &jobs.Permanent{Err: errCause}, "dead", 0, "cause"},
		{"a wrapped Permanent", fmt.Errorf("render: %w", &jobs.Permanent{Err: errCause}), "dead", 0, "render: cause"},
		{"a Permanent with no error", &jobs.Permanent{}, "dead", 0, "jobs: permanent failure"},
		{"a Retry with no error", &jobs.Retry{}, "ready", base, "jobs: retry"},
		{"a long error", errors.New("x" + strings.Repeat("é", 1000)), "ready", base, "x" + strings.Repeat("é", 511)},
		{"a nil *Retry", (*jobs.Retry)(nil), "ready", base, "jobs: retry"},
		{"a nil *Permanent", (*jobs.Permanent)(nil), "dead", 0, "jobs: permanent failure"},
		{"an Outcome of a type outside the package", embedded{&jobs.Retry{Err: errCause}}, "dead", 0, "handler returned an unknown outcome"},
	} {
		t.Run(c.name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, s *store.Store) {
				k := kind(t, "test.outcome")
				var calls atomic.Int32
				w, _ := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: func(context.Context, []byte) error {
					calls.Add(1)
					return c.err
				}}, jobs.DefaultOptions(), half)
				j := jobs.Job{Kind: k, Key: key(t, "outcome")}
				enqueue(t, s, j)

				attempted := time.Now()
				stop := start(t, w)
				time.Sleep(time.Millisecond)
				stopped(t, stop)

				if calls.Load() != 1 {
					t.Fatalf("the handler ran %d times, want once", calls.Load())
				}
				row := onlyRow(t, s)
				wantRow(t, row, c.state, 1, c.lastError)
				if c.state == "ready" {
					if want := attempted.Add(c.after).UnixMilli(); row.RunAt != want {
						t.Errorf("the retry is due at %v, want %v, %v after the attempt",
							time.UnixMilli(row.RunAt).UTC(), time.UnixMilli(want).UTC(), c.after)
					}
				}
				if len(row.LastError.String) > 1024 || !utf8.ValidString(row.LastError.String) {
					t.Errorf("last error of %d bytes, valid UTF-8 %t; want at most 1 KiB of valid UTF-8",
						len(row.LastError.String), utf8.ValidString(row.LastError.String))
				}
				if enqueue(t, s, j) {
					t.Error("the job's key was free again after its attempt")
				}
			})
		})
	}
}

// embedded is an Outcome the jobs package does not declare: embedding a
// variant gives it the unexported marker method. Only a bug would make
// one, and the worker treats it as a bug.
type embedded struct {
	*jobs.Retry
}

func TestRetryJitterComesFromRnd(t *testing.T) {
	for _, c := range []struct {
		name   string
		rnd    fixedRand
		u      float64
		failed bool // the source cannot be read
	}{
		{"the lowest number", lowest, 0, false},
		{"the middle", half, 0.5, false},
		{"the highest number", highest, 1 - 0x1p-53, false},
		{"a source that fails", fixedRand{}, 0.5, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, s *store.Store) {
				k := kind(t, "test.jitter")
				handlers := map[jobs.Kind]jobs.Handler{k: func(context.Context, []byte) error {
					return errors.New("try later")
				}}
				log, rec := newLogger()
				var w *jobs.Worker
				var err error
				if c.failed {
					w, err = jobs.NewWorker(s, handlers, time.Now, failingRand{}, log, jobs.DefaultOptions())
				} else {
					w, err = jobs.NewWorker(s, handlers, time.Now, c.rnd, log, jobs.DefaultOptions())
				}
				if err != nil {
					t.Fatal(err)
				}
				enqueue(t, s, jobs.Job{Kind: k})

				attempted := time.Now()
				stop := start(t, w)
				time.Sleep(time.Millisecond)
				stopped(t, stop)

				want := attempted.Add(jobs.Backoff(1, at(c.u))).UnixMilli()
				if row := onlyRow(t, s); row.RunAt != want {
					t.Errorf("the retry is due at %d, want %d", row.RunAt, want)
				}
				warned := rec.find("job retry jitter unavailable")
				if c.failed && len(warned) != 1 {
					t.Errorf("%d warnings for the unreadable source, want 1", len(warned))
				}
				if !c.failed && len(warned) != 0 {
					t.Errorf("%d warnings, want none", len(warned))
				}
			})
		})
	}
}

func TestRunDrainsOnShutdown(t *testing.T) {
	t.Run("a handler that finishes within the deadline", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			slow, later := kind(t, "test.slow"), kind(t, "test.later")
			entered := make(chan struct{})
			finished := make(chan error, 1)
			w, _ := newWorker(t, s, map[jobs.Kind]jobs.Handler{
				slow: func(ctx context.Context, _ []byte) error {
					close(entered)
					select {
					case <-time.After(3 * time.Second):
					case <-ctx.Done():
					}
					finished <- ctx.Err()
					return ctx.Err()
				},
				later: func(context.Context, []byte) error {
					t.Error("a job was claimed after Run's context ended")
					return nil
				},
			}, jobs.DefaultOptions(), half)
			enqueue(t, s, jobs.Job{Kind: slow})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ran := make(chan error, 1)
			go func() { ran <- w.Run(ctx) }()
			<-entered
			cancel()
			stopping := time.Now()
			// A job enqueued once Run's context has ended stays queued,
			// though the workers are told of it.
			enqueue(t, s, jobs.Job{Kind: later})
			w.Notify()
			if err := <-ran; err != nil {
				t.Fatalf("Run: %v", err)
			}
			if took := time.Since(stopping); took != 3*time.Second {
				t.Errorf("Run returned %v after its context ended, want 3s, once the handler finished", took)
			}
			if err := <-finished; err != nil {
				t.Errorf("the handler's context ended with %v during the drain, want it live", err)
			}
			all := jobRows(t, s)
			if len(all) != 2 {
				t.Fatalf("%d jobs, want 2", len(all))
			}
			wantRow(t, all[0], "done", 1, "")
			wantRow(t, all[1], "ready", 0, "")
		})
	})

	t.Run("a handler past the deadline", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			k := kind(t, "test.endless")
			entered := make(chan struct{})
			opts := jobs.DefaultOptions()
			w, log := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: func(ctx context.Context, _ []byte) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}}, opts, half)
			enqueue(t, s, jobs.Job{Kind: k})

			stop := start(t, w)
			<-entered
			stopping := time.Now()
			stopped(t, stop)
			if took := time.Since(stopping); took != opts.Drain {
				t.Errorf("Run returned %v after its context ended, want the drain deadline, %v", took, opts.Drain)
			}
			// Cut off, the handler returned its context's error: a retry,
			// recorded although Run's context had ended.
			wantRow(t, onlyRow(t, s), "ready", 1, context.Canceled.Error())
			if got := log.find("job drain deadline passed"); len(got) != 1 || got[0].level != slog.LevelWarn {
				t.Errorf("logged %+v, want one warning", got)
			}
		})
	})
}

func TestNotifyWakesWorkers(t *testing.T) {
	inBubble(t, func(t *testing.T, s *store.Store) {
		k := kind(t, "test.woken")
		opts := jobs.DefaultOptions()
		opts.Poll = time.Hour
		started := make(chan time.Time, opts.Workers)
		release := make(chan struct{})
		w, _ := newWorker(t, s, map[jobs.Kind]jobs.Handler{k: func(context.Context, []byte) error {
			started <- time.Now()
			<-release
			return nil
		}}, opts, half)

		stop := start(t, w)
		synctest.Wait() // every worker has found nothing, and waits
		woken := time.Now()
		for range opts.Workers {
			enqueue(t, s, jobs.Job{Kind: k})
		}
		w.Notify() // once, for them all
		for range opts.Workers {
			if when := <-started; !when.Equal(woken) {
				t.Errorf("a job started %v after Notify, want at once, on every idle worker", when.Sub(woken))
			}
		}
		close(release)
		stopped(t, stop)
		for _, row := range jobRows(t, s) {
			wantRow(t, row, "done", 1, "")
		}

		// Notify never blocks, with Run stopped or never started.
		for range 10 {
			w.Notify()
		}
		var zero jobs.Worker
		zero.Notify()
	})
}

func TestNewWorkerCopiesHandlers(t *testing.T) {
	inBubble(t, func(t *testing.T, s *store.Store) {
		k := kind(t, "test.copied")
		var ran atomic.Int32
		handlers := map[jobs.Kind]jobs.Handler{k: func(context.Context, []byte) error {
			ran.Add(1)
			return nil
		}}
		w, _ := newWorker(t, s, handlers, jobs.DefaultOptions(), half)
		delete(handlers, k)
		enqueue(t, s, jobs.Job{Kind: k})

		stop := start(t, w)
		time.Sleep(time.Second)
		stopped(t, stop)
		if ran.Load() != 1 {
			t.Errorf("the handler ran %d times, want once: the Worker keeps its own map", ran.Load())
		}
	})
}

func TestRunRefusesSecondCall(t *testing.T) {
	inBubble(t, func(t *testing.T, s *store.Store) {
		w, _ := newWorker(t, s, nil, jobs.DefaultOptions(), half)
		stop := start(t, w)
		synctest.Wait()
		if err := w.Run(t.Context()); err == nil {
			t.Error("a second Run while the first runs returned nil, want an error")
		}
		stopped(t, stop)

		// Once the first has returned, the Worker may run again.
		stopped(t, start(t, w))

		var zero jobs.Worker
		if err := zero.Run(t.Context()); err == nil {
			t.Error("Run on the zero Worker returned nil, want an error")
		}
	})
}

func TestNewWorkerRefusesInvalid(t *testing.T) {
	s := newStore(t)
	log, _ := newLogger()
	ok := func(context.Context, []byte) error { return nil }
	handlers := func() map[jobs.Kind]jobs.Handler {
		return map[jobs.Kind]jobs.Handler{kind(t, "test.ok"): ok}
	}
	if _, err := jobs.NewWorker(s, handlers(), time.Now, half, log, jobs.DefaultOptions()); err != nil {
		t.Fatalf("NewWorker: %v, want it accepted", err)
	}
	if _, err := jobs.NewWorker(s, nil, time.Now, half, log, jobs.DefaultOptions()); err != nil {
		t.Errorf("NewWorker with no handlers: %v, want it accepted", err)
	}

	cases := map[string]func() (*jobs.Worker, error){
		"no store": func() (*jobs.Worker, error) {
			return jobs.NewWorker(nil, handlers(), time.Now, half, log, jobs.DefaultOptions())
		},
		"a store not opened": func() (*jobs.Worker, error) {
			return jobs.NewWorker(&store.Store{}, handlers(), time.Now, half, log, jobs.DefaultOptions())
		},
		"a nil handler": func() (*jobs.Worker, error) {
			h := handlers()
			h[kind(t, "test.nil")] = nil
			return jobs.NewWorker(s, h, time.Now, half, log, jobs.DefaultOptions())
		},
		"the zero Kind": func() (*jobs.Worker, error) {
			h := handlers()
			h[jobs.Kind{}] = ok
			return jobs.NewWorker(s, h, time.Now, half, log, jobs.DefaultOptions())
		},
		"no clock": func() (*jobs.Worker, error) {
			return jobs.NewWorker(s, handlers(), nil, half, log, jobs.DefaultOptions())
		},
		"no random source": func() (*jobs.Worker, error) {
			return jobs.NewWorker(s, handlers(), time.Now, nil, log, jobs.DefaultOptions())
		},
		"no logger": func() (*jobs.Worker, error) {
			return jobs.NewWorker(s, handlers(), time.Now, half, nil, jobs.DefaultOptions())
		},
	}
	for name, change := range map[string]func(*jobs.Options){
		"the zero Options":            func(o *jobs.Options) { *o = jobs.Options{} },
		"no workers":                  func(o *jobs.Options) { o.Workers = 0 },
		"negative workers":            func(o *jobs.Options) { o.Workers = -1 },
		"no lease":                    func(o *jobs.Options) { o.Lease = 0 },
		"no backoff base":             func(o *jobs.Options) { o.BackoffBase = 0 },
		"a cap below the base":        func(o *jobs.Options) { o.BackoffCap = o.BackoffBase - 1 },
		"negative jitter":             func(o *jobs.Options) { o.Jitter = -0.1 },
		"jitter of the whole delay":   func(o *jobs.Options) { o.Jitter = 1 },
		"jitter that is not a number": func(o *jobs.Options) { o.Jitter = math.NaN() },
		"no attempts":                 func(o *jobs.Options) { o.MaxAttempts = 0 },
		"no poll":                     func(o *jobs.Options) { o.Poll = 0 },
		"no drain":                    func(o *jobs.Options) { o.Drain = 0 },
		"a negative drain":            func(o *jobs.Options) { o.Drain = -time.Second },
	} {
		cases[name] = func() (*jobs.Worker, error) {
			o := jobs.DefaultOptions()
			change(&o)
			return jobs.NewWorker(s, handlers(), time.Now, half, log, o)
		}
	}
	for name, newWorker := range cases {
		if w, err := newWorker(); !errors.Is(err, jobs.ErrInvalid) || w != nil {
			t.Errorf("NewWorker with %s = %v, %v; want nil and an error wrapping ErrInvalid", name, w, err)
		}
	}

	// Every problem is reported at once.
	_, err := jobs.NewWorker(nil, map[jobs.Kind]jobs.Handler{{}: nil}, nil, nil, nil, jobs.Options{})
	joined, isJoined := errors.AsType[interface {
		error
		Unwrap() []error
	}](err)
	if !isJoined || len(joined.Unwrap()) < 6 {
		t.Errorf("NewWorker with everything wrong: %v; want each problem reported", err)
	}
}
