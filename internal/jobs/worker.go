// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// Handler runs one job, given its payload, and returns nil once the job is
// done. Otherwise its error says what follows: a *Permanent moves the job
// to the dead letter, and a *Retry or any other error has it tried again,
// unless it has had its last attempt. The Worker looks for an Outcome
// anywhere in the error's chain, so a Handler may wrap one. A Handler
// that panics has its job moved to the dead letter: a panic is a bug,
// which trying again would only repeat. The error's message is logged and
// kept with the job, so it must not hold the payload or personal data.
//
// ctx ends when the job's lease does, or, once Run's context has ended,
// when the drain deadline passes, and a Handler returns promptly then. A
// job may run more than once (see the package doc), so a Handler must be
// idempotent.
type Handler func(ctx context.Context, payload []byte) error

// Outcome is how a failing Handler says what should follow: a *Retry or a
// *Permanent, which the Handler returns as its error or wraps in it.
//
//sumtype:decl
type Outcome interface {
	error
	outcome()
}

// Retry has a job tried again after After, when After is positive, and
// otherwise after the backoff any error gets. A wait past the Worker's
// Options.BackoffCap is cut to the cap. A job that has had its last
// attempt goes to the dead letter instead, as for any error. Err is why
// the attempt failed, and may be nil.
type Retry struct {
	After time.Duration
	Err   error
}

// Error returns Err's message, or "jobs: retry" when Err is nil.
func (r *Retry) Error() string {
	if r == nil || r.Err == nil {
		return "jobs: retry"
	}
	return r.Err.Error()
}

// Unwrap returns Err.
func (r *Retry) Unwrap() error {
	if r == nil {
		return nil
	}
	return r.Err
}

func (*Retry) outcome() {}

// Permanent moves a job to the dead letter at once, as no attempt can
// succeed. Err is why, and may be nil.
type Permanent struct {
	Err error
}

// Error returns Err's message, or "jobs: permanent failure" when Err is
// nil.
func (p *Permanent) Error() string {
	if p == nil || p.Err == nil {
		return "jobs: permanent failure"
	}
	return p.Err.Error()
}

// Unwrap returns Err.
func (p *Permanent) Unwrap() error {
	if p == nil {
		return nil
	}
	return p.Err
}

func (*Permanent) outcome() {}

// Options are how a Worker runs. NewWorker refuses Options with any field
// outside its range, the zero Options included. DefaultOptions returns
// the values Pisati runs with.
type Options struct {
	// Workers is how many jobs run at once, each on a goroutine of its
	// own. At least 1.
	Workers int
	// Lease is how long a claimed job stays the claiming worker's. Once it
	// expires, another worker may claim the job, and the Handler's context
	// ends. Positive.
	Lease time.Duration
	// BackoffBase is how long a job waits after its first failed attempt,
	// doubling after each attempt after it up to BackoffCap. Positive,
	// with BackoffCap no shorter.
	BackoffBase, BackoffCap time.Duration
	// Jitter moves each wait by up to this fraction of it either way, so
	// that jobs that failed together are not all tried again together.
	// In [0, 1).
	Jitter float64
	// MaxAttempts is how many attempts a job has before it goes to the
	// dead letter. At least 1.
	MaxAttempts int
	// Poll is how long an idle worker waits before it looks for a due job
	// again, unless Notify wakes it sooner. Positive.
	Poll time.Duration
	// Drain is how long the Handlers running when Run's context ends may
	// go on before their contexts end too. Positive.
	Drain time.Duration
}

// DefaultOptions returns the Options Pisati runs with: 4 workers, a 5 min
// lease, a backoff of 30 s doubling to 6 h with ±20% jitter, 12 attempts,
// a poll every second, and an 8 s drain.
func DefaultOptions() Options {
	return Options{
		Workers:     4,
		Lease:       5 * time.Minute,
		BackoffBase: 30 * time.Second,
		BackoffCap:  6 * time.Hour,
		Jitter:      0.2,
		MaxAttempts: 12,
		Poll:        time.Second,
		Drain:       8 * time.Second,
	}
}

// problems returns an error for each field of o outside its range.
func (o Options) problems() []error {
	var errs []error
	if o.Workers < 1 {
		errs = append(errs, fmt.Errorf("jobs: %w options: Workers is %d, want at least 1", ErrInvalid, o.Workers))
	}
	if o.MaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("jobs: %w options: MaxAttempts is %d, want at least 1", ErrInvalid, o.MaxAttempts))
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"Lease", o.Lease},
		{"BackoffBase", o.BackoffBase},
		{"Poll", o.Poll},
		{"Drain", o.Drain},
	} {
		if d.value <= 0 {
			errs = append(errs, fmt.Errorf("jobs: %w options: %s is %v, want it positive", ErrInvalid, d.name, d.value))
		}
	}
	if o.BackoffCap < o.BackoffBase {
		errs = append(errs, fmt.Errorf("jobs: %w options: BackoffCap %v is shorter than BackoffBase %v",
			ErrInvalid, o.BackoffCap, o.BackoffBase))
	}
	if math.IsNaN(o.Jitter) || o.Jitter < 0 || o.Jitter >= 1 {
		errs = append(errs, fmt.Errorf("jobs: %w options: Jitter is %v, want it in [0, 1)", ErrInvalid, o.Jitter))
	}
	return errs
}

// Worker runs the jobs queued in a Store with the Handler for each job's
// Kind. Make one with NewWorker. The zero Worker runs nothing: its Run
// returns an error and its Notify does nothing.
type Worker struct {
	store    *store.Store
	handlers map[Kind]Handler
	clock    func() time.Time
	log      *slog.Logger
	opts     Options

	// wake holds at most one token, so that Notify never blocks, and a
	// wake that comes while every worker is busy is kept for the first to
	// go idle.
	wake chan struct{}
	// running is set while Run runs, so that a second Run is refused.
	running atomic.Bool

	// rndMu makes reads of rnd one at a time: an io.Reader need not be
	// safe for concurrent use.
	rndMu sync.Mutex
	rnd   io.Reader
}

// NewWorker returns a Worker that runs the jobs queued in s, each with the
// Handler h holds for its Kind; it keeps a copy of h, so later changes to
// h do not reach it. A job of a Kind h has no Handler for goes to the
// dead letter.
//
// clock gives the time every lease, due time and retry is measured from.
// rnd is the source of the backoff's jitter, crypto/rand.Reader in Pisati;
// the Worker reads it from one goroutine at a time. A retry for which rnd
// cannot be read is logged, and gets no jitter. log receives the Worker's
// events, which never include a payload.
//
// NewWorker returns an error wrapping ErrInvalid, naming every problem at
// once, when s is not open, h holds a nil Handler or one for the zero
// Kind, clock, rnd or log is nil, or opts has a field outside its range.
func NewWorker(s *store.Store, h map[Kind]Handler, clock func() time.Time, rnd io.Reader, log *slog.Logger, opts Options) (*Worker, error) {
	var errs []error
	if s.DB() == nil {
		errs = append(errs, fmt.Errorf("jobs: %w store: it is not open", ErrInvalid))
	}
	for k, handler := range h {
		if k == (Kind{}) {
			errs = append(errs, fmt.Errorf("jobs: %w handlers: one is for the zero Kind", ErrInvalid))
		}
		if handler == nil {
			errs = append(errs, fmt.Errorf("jobs: %w handlers: the one for %q is nil", ErrInvalid, k))
		}
	}
	if clock == nil {
		errs = append(errs, fmt.Errorf("jobs: %w clock: nil", ErrInvalid))
	}
	if rnd == nil {
		errs = append(errs, fmt.Errorf("jobs: %w random source: nil", ErrInvalid))
	}
	if log == nil {
		errs = append(errs, fmt.Errorf("jobs: %w logger: nil", ErrInvalid))
	}
	errs = append(errs, opts.problems()...)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Worker{
		store:    s,
		handlers: maps.Clone(h),
		clock:    clock,
		log:      log,
		opts:     opts,
		wake:     make(chan struct{}, 1),
		rnd:      rnd,
	}, nil
}

// Run runs jobs until ctx ends. Each of Options.Workers goroutines claims
// the job due first, leases it for Options.Lease, runs its Handler and
// records the outcome, then claims the next; one that finds no job due
// waits for Options.Poll, or until Notify wakes it. A claim or a record
// that fails is logged, and the worker carries on.
//
// Once ctx ends, no job is claimed. The Handlers still running go on under
// context.WithoutCancel(ctx) for up to Options.Drain, after which their
// contexts end. Each outcome is recorded even after ctx has ended, a write
// that SQLite's 5 s busy timeout bounds, so that a job cut off is ready to
// retry at once rather than left leased. Run returns when every goroutine
// it started has, so it waits for each Handler to return once its context
// ends.
//
// Run returns nil once it has stopped because ctx ended, even when the
// drain deadline cut Handlers off: their jobs are retried like any failed
// attempt. It returns an error at once, running nothing, when the Worker
// was not made by NewWorker or another Run of it has not returned.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil || w.store == nil {
		return errors.New("jobs: run: the Worker was not made by NewWorker")
	}
	if !w.running.CompareAndSwap(false, true) {
		return errors.New("jobs: run: the Worker is running already")
	}
	defer w.running.Store(false)

	work, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWork()
	var workers sync.WaitGroup
	for range w.opts.Workers {
		workers.Go(func() { w.work(ctx, work) })
	}
	finished := make(chan struct{})
	go func() {
		workers.Wait()
		close(finished)
	}()

	// The workers return only once ctx has ended.
	<-ctx.Done()
	drain := time.NewTimer(w.opts.Drain)
	defer drain.Stop()
	select {
	case <-finished:
	case <-drain.C:
		w.log.WarnContext(ctx, "job drain deadline passed", "drain", w.opts.Drain)
		stopWork()
		<-finished
	}
	return nil
}

// Notify wakes an idle worker of a running Run to claim at once, rather
// than at its next poll. A worker that claims a job wakes another in
// turn, so one Notify reaches as many idle workers as there are jobs due.
// Call it once the transaction that enqueued a job has committed. It never
// blocks: when no worker is idle, the first to finish its job claims
// again at once. The zero Worker's Notify does nothing.
func (w *Worker) Notify() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// work claims and runs jobs until ctx ends, running each Handler under
// handlers, the context that outlives ctx by the drain.
func (w *Worker) work(ctx, handlers context.Context) {
	for ctx.Err() == nil {
		j, ok := w.claim(ctx)
		if !ok {
			w.idle(ctx)
			continue
		}
		// Pass the wake on, so that the jobs one Notify announced reach
		// every idle worker rather than this one alone.
		w.Notify()
		w.process(handlers, j)
	}
}

// idle waits for the poll interval, a wake from Notify, or ctx's end.
func (w *Worker) idle(ctx context.Context) {
	poll := time.NewTimer(w.opts.Poll)
	defer poll.Stop()
	select {
	case <-ctx.Done():
	case <-w.wake:
	case <-poll.C:
	}
}

// claim leases the job due first, in one IMMEDIATE transaction, and
// reports whether there was one.
func (w *Worker) claim(ctx context.Context) (storedb.ClaimJobRow, bool) {
	now := w.clock()
	var j storedb.ClaimJobRow
	err := w.store.InTx(ctx, func(q *storedb.Queries) error {
		var err error
		j, err = q.ClaimJob(ctx, storedb.ClaimJobParams{
			Now:        now.UnixMilli(),
			LeaseUntil: sql.NullInt64{Int64: now.Add(w.opts.Lease).UnixMilli(), Valid: true},
		})
		return err
	})
	switch {
	case err == nil:
		return j, true
	case errors.Is(err, sql.ErrNoRows), ctx.Err() != nil:
		// No job is due, or Run is stopping: a claim its context cut off
		// was rolled back.
		return storedb.ClaimJobRow{}, false
	}
	w.log.ErrorContext(ctx, "job claim failed", "error", err)
	return storedb.ClaimJobRow{}, false
}

// process runs the job j has leased, under ctx, and records the outcome.
func (w *Worker) process(ctx context.Context, j storedb.ClaimJobRow) {
	// Recorded under a context that does not end with the drain: an
	// outcome recorded beats a lease left to expire.
	rec := context.WithoutCancel(ctx)
	if j.Attempts > int64(w.opts.MaxAttempts) {
		// Only a lease that expired, on the last attempt, comes back to
		// a claim with no attempts left.
		w.bury(rec, j, "no attempts left: the last attempt's lease expired")
		return
	}
	h, ok := w.handler(j.Kind)
	if !ok {
		if w.deadLetter(rec, j, "no handler for job kind "+j.Kind) {
			w.log.ErrorContext(rec, "job kind has no handler", "kind", j.Kind, "job_id", j.ID)
		}
		return
	}
	w.record(rec, j, w.call(ctx, h, j))
}

// handler returns the Handler for the kind named name.
func (w *Worker) handler(name string) (Handler, bool) {
	k, err := NewKind(name)
	if err != nil {
		return nil, false
	}
	h, ok := w.handlers[k]
	return h, ok
}

// errPanicked is what call returns for a Handler that panicked. No Handler
// can return it, as it is not exported.
var errPanicked = errors.New("handler panicked")

// call runs h on j's payload under a context that ends with j's lease.
// This is the top of the job's run, so it recovers a panic in h, which it
// logs and returns as errPanicked.
func (w *Worker) call(ctx context.Context, h Handler, j storedb.ClaimJobRow) (err error) {
	ctx, cancel := context.WithTimeout(ctx, w.opts.Lease)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			w.log.ErrorContext(ctx, "job handler panicked", "kind", j.Kind, "job_id", j.ID,
				"attempt", j.Attempts, "panic", describePanic(r), "stack", string(debug.Stack()))
			err = errPanicked
		}
	}()
	return h(ctx, j.Payload)
}

// describePanic describes the value a Handler panicked with, for the log.
// A runtime error's message holds no data of the job's. Any other value
// might hold the payload, so only its type is given.
func describePanic(r any) string {
	if re, ok := r.(runtime.Error); ok {
		return re.Error()
	}
	return fmt.Sprintf("a value of type %T", r)
}

// record records the outcome of j's attempt, given err, what its Handler
// returned.
func (w *Worker) record(ctx context.Context, j storedb.ClaimJobRow, err error) {
	if err == nil {
		if w.settle(ctx, j, func(q *storedb.Queries) (int64, error) {
			return q.CompleteJob(ctx, storedb.CompleteJobParams{ID: j.ID, Attempts: j.Attempts})
		}) {
			w.log.DebugContext(ctx, "job done", "kind", j.Kind, "job_id", j.ID, "attempt", j.Attempts)
		}
		return
	}
	if errors.Is(err, errPanicked) {
		// call has logged it.
		w.deadLetter(ctx, j, errPanicked.Error())
		return
	}
	outcome, ok := errors.AsType[Outcome](err)
	if !ok {
		w.retry(ctx, j, err, 0)
		return
	}
	switch o := outcome.(type) {
	case *Retry:
		var after time.Duration
		if o != nil {
			after = o.After
		}
		w.retry(ctx, j, err, after)
	case *Permanent:
		w.bury(ctx, j, err.Error())
	default:
		// Only a type outside the package that embeds a variant reaches
		// here: a bug, which trying again would only repeat.
		if w.deadLetter(ctx, j, "handler returned an unknown outcome") {
			w.log.ErrorContext(ctx, "job handler returned an unknown outcome", "kind", j.Kind,
				"job_id", j.ID, "attempt", j.Attempts, "type", fmt.Sprintf("%T", o))
		}
	}
}

// retry makes j ready again after after, when it is positive, or else
// after the backoff for its attempts; or, when j has had its last attempt,
// moves it to the dead letter. err is why the attempt failed.
func (w *Worker) retry(ctx context.Context, j storedb.ClaimJobRow, err error, after time.Duration) {
	if j.Attempts >= int64(w.opts.MaxAttempts) {
		w.bury(ctx, j, err.Error())
		return
	}
	delay := min(after, w.opts.BackoffCap)
	if after <= 0 {
		delay = w.opts.backoff(j.Attempts, w.jitter(ctx))
	}
	why := lastError(err.Error())
	if w.settle(ctx, j, func(q *storedb.Queries) (int64, error) {
		return q.RetryJob(ctx, storedb.RetryJobParams{
			RunAt:     w.clock().Add(delay).UnixMilli(),
			LastError: why,
			ID:        j.ID,
			Attempts:  j.Attempts,
		})
	}) {
		w.log.WarnContext(ctx, "job retry scheduled", "kind", j.Kind, "job_id", j.ID,
			"attempt", j.Attempts, "retry_in", delay, "error", why.String)
	}
}

// bury moves j to the dead letter with why as its last error, and logs
// that it did.
func (w *Worker) bury(ctx context.Context, j storedb.ClaimJobRow, why string) {
	if w.deadLetter(ctx, j, why) {
		w.log.ErrorContext(ctx, "job dead-lettered", "kind", j.Kind, "job_id", j.ID,
			"attempt", j.Attempts, "error", lastError(why).String)
	}
}

// deadLetter moves j to the dead letter with why as its last error, and
// reports whether j's lease was still its own to do so.
func (w *Worker) deadLetter(ctx context.Context, j storedb.ClaimJobRow, why string) bool {
	return w.settle(ctx, j, func(q *storedb.Queries) (int64, error) {
		return q.DeadLetterJob(ctx, storedb.DeadLetterJobParams{LastError: lastError(why), ID: j.ID, Attempts: j.Attempts})
	})
}

// settle runs update, which records j's outcome if j's lease is still its
// own and returns the rows it changed, in one transaction. It reports
// whether the outcome was recorded, and logs why when it was not: the
// write failed, or the lease expired and another claim took the job.
func (w *Worker) settle(ctx context.Context, j storedb.ClaimJobRow, update func(*storedb.Queries) (int64, error)) bool {
	var changed int64
	err := w.store.InTx(ctx, func(q *storedb.Queries) error {
		var err error
		changed, err = update(q)
		return err
	})
	switch {
	case err != nil:
		w.log.ErrorContext(ctx, "job outcome not recorded", "kind", j.Kind, "job_id", j.ID,
			"attempt", j.Attempts, "error", err)
		return false
	case changed == 0:
		w.log.WarnContext(ctx, "job lease lost", "kind", j.Kind, "job_id", j.ID, "attempt", j.Attempts)
		return false
	}
	return true
}

// jitter returns, for the backoff, a function giving a number in [0, 1)
// read from the Worker's random source, or nil, for no jitter, when the
// source cannot be read.
func (w *Worker) jitter(ctx context.Context) func() float64 {
	var b [8]byte
	w.rndMu.Lock()
	_, err := io.ReadFull(w.rnd, b[:])
	w.rndMu.Unlock()
	if err != nil {
		w.log.WarnContext(ctx, "job retry jitter unavailable", "error", err)
		return nil
	}
	// The top 53 bits over 2^53: exact as a float64, and below 1.
	u := float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
	return func() float64 { return u }
}

// maxLastError bounds the error recorded with a job, in bytes.
const maxLastError = 1 << 10

// lastError returns msg as a job's last error: cut to at most 1 KiB, at
// the start of a character, so that valid UTF-8 stays valid.
func lastError(msg string) sql.NullString {
	if len(msg) > maxLastError {
		cut := maxLastError
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut]
	}
	return sql.NullString{String: msg, Valid: true}
}
