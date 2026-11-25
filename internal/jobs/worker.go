// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs

import (
	"cmp"
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
	"slices"
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
// when the handlers' share of the drain passes (Options.Drain less
// Options.Reserve), and a Handler returns promptly then. A
// job may run more than once (see the package doc), so a Handler must be
// idempotent. A Worker may run one Kind's Handler on up to
// Options.Workers goroutines at once, so a Handler must also be safe for
// concurrent use.
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

// Error returns Err's message, or one that says the job gave no reason
// when Err is nil.
func (r *Retry) Error() string {
	if r == nil || r.Err == nil {
		return "the job asked to be tried again, and gave no reason"
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

// Error returns Err's message, or one that says the job gave no reason
// when Err is nil.
func (p *Permanent) Error() string {
	if p == nil || p.Err == nil {
		return "the job failed in a way that trying again cannot fix, and gave no reason"
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
	// Drain is how long Run takes at most to stop, once its context
	// ends: the Handlers then running may go on for Drain less Reserve,
	// after which their contexts end, and Reserve is left to record
	// their outcomes. Positive.
	Drain time.Duration
	// Reserve is the end of Drain kept for recording the outcomes of the
	// Handlers cut off, so that each is recorded within Drain. Positive,
	// and shorter than Drain.
	Reserve time.Duration
	// SweepEvery is how often Run deletes the done and dead jobs past
	// their retention, after a first sweep as it starts. Positive.
	SweepEvery time.Duration
	// KeepDone is how long a job is kept once it is done, and KeepDead how
	// long once it is dead, before a sweep deletes it. A job's idempotency
	// key stays taken until then. Positive.
	KeepDone, KeepDead time.Duration
}

// DefaultOptions returns the Options Pisati runs with: 4 workers, a 5 min
// lease, a backoff of 30 s doubling to 6 h with ±20% jitter, 12 attempts,
// a poll every second, an 8 s drain whose last second is kept for
// recording outcomes, and a sweep every 10 minutes that keeps done jobs
// for 7 days and dead ones for 30.
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
		Reserve:     time.Second,
		SweepEvery:  10 * time.Minute,
		KeepDone:    7 * 24 * time.Hour,
		KeepDead:    30 * 24 * time.Hour,
	}
}

// problems returns an error for each field of o outside its range.
func (o Options) problems() []error {
	var errs []error
	if o.Workers < 1 {
		errs = append(errs, badOptions("Workers is %d, but it must be at least 1", o.Workers))
	}
	if o.MaxAttempts < 1 {
		errs = append(errs, badOptions("MaxAttempts is %d, but it must be at least 1", o.MaxAttempts))
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"Lease", o.Lease},
		{"BackoffBase", o.BackoffBase},
		{"Poll", o.Poll},
		{"Drain", o.Drain},
		{"Reserve", o.Reserve},
		{"SweepEvery", o.SweepEvery},
		{"KeepDone", o.KeepDone},
		{"KeepDead", o.KeepDead},
	} {
		if d.value <= 0 {
			errs = append(errs, badOptions("%s is %v, but it must be longer than 0", d.name, d.value))
		}
	}
	if o.BackoffCap < o.BackoffBase {
		errs = append(errs, badOptions("BackoffCap is %v, but it must be no shorter than BackoffBase, %v",
			o.BackoffCap, o.BackoffBase))
	}
	if o.Reserve > 0 && o.Reserve >= o.Drain {
		errs = append(errs, badOptions("Reserve is %v, but it must be shorter than Drain, %v", o.Reserve, o.Drain))
	}
	if math.IsNaN(o.Jitter) || o.Jitter < 0 || o.Jitter >= 1 {
		errs = append(errs, badOptions("Jitter is %v, but it must be at least 0 and below 1", o.Jitter))
	}
	return errs
}

// badOptions is the error for Options whose field breaks its rule, as
// format and args say: a bug in Pisati, which gives its Options in code.
func badOptions(format string, args ...any) error {
	return explain(ErrInvalid, "jobs.NewWorker was given Options whose %s, which is a bug in Pisati",
		fmt.Sprintf(format, args...))
}

// plainError is an error whose message is Pisati's own, worded for the
// operator, around err, which errors.Is and errors.AsType still find.
type plainError struct {
	msg string
	err error
}

func (e *plainError) Error() string { return e.msg }
func (e *plainError) Unwrap() error { return e.err }

// explain returns err behind the message format and args give.
func explain(err error, format string, args ...any) error {
	return &plainError{msg: fmt.Sprintf(format, args...), err: err}
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
	// running is set while Run's goroutines run, so that a second Run is
	// refused, even once Run has returned at the stop deadline without
	// them.
	running atomic.Bool

	// activeMu guards active, the tries being processed, by try: what
	// Run names when it stops without them. A job claimed again while an
	// earlier try still runs has two.
	activeMu sync.Mutex
	active   map[try]storedb.ClaimJobRow

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
// clock gives the time every lease, due time, retry and retention is
// measured from. The Worker calls it from several goroutines at once, so
// it must be safe for concurrent use, as time.Now is. rnd is the source of the backoff's jitter, crypto/rand.Reader in Pisati;
// the Worker reads it from one goroutine at a time. A retry for which rnd
// cannot be read is logged, and gets no jitter. log receives the Worker's
// events, which never include a payload.
//
// NewWorker returns an error wrapping ErrInvalid, naming every problem at
// once, when s is not open, h holds a nil Handler or one for the zero
// Kind, clock, rnd or log is nil, or opts has a field outside its range.
func NewWorker(s *store.Store, h map[Kind]Handler, clock func() time.Time, rnd io.Reader, log *slog.Logger, opts Options) (*Worker, error) {
	var errs []error
	const bug = "which is a bug in Pisati"
	if s.DB() == nil {
		errs = append(errs, explain(ErrInvalid, "jobs.NewWorker was given a store that is not open, %s", bug))
	}
	for k, handler := range h {
		if k == (Kind{}) {
			errs = append(errs, explain(ErrInvalid, "jobs.NewWorker was given a handler for the zero Kind, %s", bug))
		}
		if handler == nil {
			errs = append(errs, explain(ErrInvalid, "jobs.NewWorker was given a nil handler for the kind %q, %s", k, bug))
		}
	}
	if clock == nil {
		errs = append(errs, explain(ErrInvalid, "jobs.NewWorker was given no clock, %s", bug))
	}
	if rnd == nil {
		errs = append(errs, explain(ErrInvalid, "jobs.NewWorker was given no random source, %s", bug))
	}
	if log == nil {
		errs = append(errs, explain(ErrInvalid, "jobs.NewWorker was given no logger, %s", bug))
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
		active:   map[try]storedb.ClaimJobRow{},
	}, nil
}

// Run runs jobs until ctx ends. Each of Options.Workers goroutines claims
// the job due first, leases it for Options.Lease, runs its Handler and
// records the outcome, then claims the next; one that finds no job due
// waits for Options.Poll, or until Notify wakes it. A claim or a record
// that fails is logged, and the worker carries on. Run first deletes the
// done and dead jobs kept past Options.KeepDone and Options.KeepDead,
// before any claim, and then again every Options.SweepEvery, a bounded
// batch to a transaction so that no sweep holds the write lock for long.
//
// Once ctx ends, no job is claimed and no sweep starts, and Run stops
// within Options.Drain. The Handlers still running go on under
// context.WithoutCancel(ctx) for Drain less Options.Reserve, after which
// their contexts end. A job whose Handler then returns an error, of
// whatever kind, is given back as a free retry: ready at once, due when
// it was, its attempt not counted, as being cut off says nothing of the
// job. A Handler that returns nil all the same has its job done, and one
// that panics, dead. Each outcome is recorded even after ctx has ended,
// under a context that ends with Drain, so that no record outlasts Run:
// one still waiting for the write lock then fails, logged, and its job
// keeps its lease.
//
// Run returns once every goroutine it started has, or once Drain has
// passed since ctx ended, whichever comes first. A try still being
// processed then, its Handler ignoring its context or its outcome not yet
// written, is logged, and its goroutine goes on without Run: the job
// keeps its lease, and ReleaseOrphans makes it ready again at the next
// start, its try counted. Pisati's process ends just after, and with it
// that goroutine. Until every goroutine of a Run has ended, another Run
// is refused.
//
// Run returns nil once it has stopped because ctx ended, even when the
// drain cut Handlers off. It returns an error at once, running nothing,
// when the Worker was not made by NewWorker or another Run of it has not
// ended.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil || w.store == nil {
		return errors.New("jobs.Worker.Run was called on a Worker that jobs.NewWorker did not make, which is a bug in Pisati")
	}
	if !w.running.CompareAndSwap(false, true) {
		return errors.New("jobs.Worker.Run was called on a Worker that is running already, which is a bug in Pisati")
	}

	// The first sweep runs before the workers start, so that it never
	// contends with their claims for the write lock.
	w.sweep(ctx)
	work, stopWork := context.WithCancelCause(context.WithoutCancel(ctx))
	defer stopWork(nil)
	// Outcomes are recorded under records, which ends with the drain, so
	// that no record outlasts Run, however long it waits for the write
	// lock.
	records, stopRecords := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRecords()
	var workers sync.WaitGroup
	workers.Go(func() { w.sweepEvery(ctx) })
	for range w.opts.Workers {
		workers.Go(func() { w.work(ctx, work, records) })
	}
	finished := make(chan struct{})
	go func() {
		workers.Wait()
		// Before finished closes, so that a Run that has seen it close
		// may be followed by another at once.
		w.running.Store(false)
		close(finished)
	}()

	// The workers and the sweeper return only once ctx has ended.
	<-ctx.Done()
	handlers := w.opts.Drain - w.opts.Reserve
	cut := time.NewTimer(handlers)
	defer cut.Stop()
	select {
	case <-finished:
		return nil
	case <-cut.C:
		w.log.WarnContext(ctx, "running jobs told to stop, so that their results can be recorded before the stop deadline",
			"after", handlers.String(), "left", w.opts.Reserve.String())
		stopWork(errDrained)
	}
	reserve := time.NewTimer(w.opts.Reserve)
	defer reserve.Stop()
	select {
	case <-finished:
	case <-reserve.C:
		for _, j := range w.stillActive() {
			w.log.ErrorContext(ctx, "job not finished by the stop deadline", "kind", j.Kind, "job_id", j.ID, "attempt", j.Attempts)
		}
		stopRecords()
	}
	return nil
}

// try names one claim of a job: its ID, and the lease token the claim
// raised it to.
type try struct {
	id, token int64
}

// stillActive returns the tries being processed, in the order of their
// jobs' IDs and then their claims.
func (w *Worker) stillActive() []storedb.ClaimJobRow {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	return slices.SortedFunc(maps.Values(w.active), func(a, b storedb.ClaimJobRow) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.LeaseToken, b.LeaseToken))
	})
}

// errDrained is the cause with which Run ends the Handlers' contexts when
// the drain deadline passes.
var errDrained = errors.New("the job was told to stop, because Pisati is stopping and the time for running jobs has passed")

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
// handlers, the context that outlives ctx by the handlers' share of the
// drain, and recording each outcome under records, which ends with the
// drain.
func (w *Worker) work(ctx, handlers, records context.Context) {
	for ctx.Err() == nil {
		j, ok := w.claim(ctx)
		if !ok {
			w.idle(ctx)
			continue
		}
		// Pass the wake on, so that the jobs one Notify announced reach
		// every idle worker rather than this one alone.
		w.Notify()
		w.process(handlers, records, j)
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
	var j storedb.ClaimJobRow
	err := w.store.InTx(ctx, func(q *storedb.Queries) error {
		// Read once the transaction holds the write lock, which it may
		// have waited up to the busy timeout for: a lease measured from
		// before the wait would end that much early.
		now := w.clock()
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
	w.log.ErrorContext(ctx, "next job not taken from the queue", "error", err)
	return storedb.ClaimJobRow{}, false
}

// process runs the job j has leased, under ctx, and records the outcome
// under rec. Until it has, this try of j is among those Run names should
// it stop without it.
func (w *Worker) process(ctx, rec context.Context, j storedb.ClaimJobRow) {
	this := try{id: j.ID, token: j.LeaseToken}
	w.activeMu.Lock()
	w.active[this] = j
	w.activeMu.Unlock()
	defer func() {
		w.activeMu.Lock()
		delete(w.active, this)
		w.activeMu.Unlock()
	}()
	if j.Attempts > int64(w.opts.MaxAttempts) {
		// A job comes back to a claim with no attempts left only when
		// its last try never recorded an outcome: its lease expired, or
		// Pisati stopped while it ran and ReleaseOrphans gave it back.
		w.bury(rec, j, "The last try did not finish: Pisati stopped while the job ran, or the try ran longer than its time limit.")
		return
	}
	h, ok := w.handler(j.Kind)
	if !ok {
		if w.deadLetter(rec, j, "This version of Pisati has no handler for jobs of the kind "+j.Kind+", so the job cannot run.") {
			w.log.ErrorContext(rec, "job given up: Pisati has no handler for its kind", "kind", j.Kind, "job_id", j.ID)
		}
		return
	}
	// The Handler's context ends when the lease written does, measured on
	// the Worker's clock, so that no other claim can take the job while it
	// runs.
	leased, cancel := context.WithTimeout(ctx, time.UnixMilli(j.LeaseUntil.Int64).Sub(w.clock()))
	err := w.call(leased, h, j)
	cutOff := errors.Is(context.Cause(leased), errDrained)
	cancel()
	if err != nil && cutOff && !errors.Is(err, errPanicked) {
		w.release(rec, j)
		return
	}
	w.record(rec, j, err)
}

// release gives j back, ready at once and its attempt uncounted: the
// drain deadline cut it off, which says nothing of the job.
func (w *Worker) release(ctx context.Context, j storedb.ClaimJobRow) {
	if w.settle(ctx, j, func(q *storedb.Queries) (int64, error) {
		return q.ReleaseJob(ctx, storedb.ReleaseJobParams{ID: j.ID, LeaseToken: j.LeaseToken})
	}) {
		w.log.InfoContext(ctx, "job given back at the stop, to run again, this try not counted", "kind", j.Kind,
			"job_id", j.ID, "attempt", j.Attempts)
	}
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

// The last errors kept with a job that Pisati gives up for a bug.
const (
	panicked       = "The job's handler crashed with a panic, which is a bug in Pisati. The log of pisati serve holds the details."
	unknownOutcome = "The job's handler returned an outcome that Pisati does not know, which is a bug in Pisati."
)

// call runs h on j's payload under ctx. This is the top of the job's run,
// so it recovers a panic in h, which it logs and returns as errPanicked.
func (w *Worker) call(ctx context.Context, h Handler, j storedb.ClaimJobRow) (err error) {
	defer func() {
		if r := recover(); r != nil {
			w.log.ErrorContext(ctx, "job handler crashed with a panic", "kind", j.Kind, "job_id", j.ID,
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
			return q.CompleteJob(ctx, storedb.CompleteJobParams{FinishedAt: w.now(), ID: j.ID, LeaseToken: j.LeaseToken})
		}) {
			w.log.DebugContext(ctx, "job done", "kind", j.Kind, "job_id", j.ID, "attempt", j.Attempts)
		}
		return
	}
	if errors.Is(err, errPanicked) {
		// call has logged it.
		w.deadLetter(ctx, j, panicked)
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
		if w.deadLetter(ctx, j, unknownOutcome) {
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
			RunAt:      w.clock().Add(delay).UnixMilli(),
			LastError:  why,
			ID:         j.ID,
			LeaseToken: j.LeaseToken,
		})
	}) {
		w.log.WarnContext(ctx, "job will be tried again", "kind", j.Kind, "job_id", j.ID,
			"attempt", j.Attempts, "retry_in", delay.String(), "error", why.String)
	}
}

// bury moves j to the dead letter with why as its last error, and logs
// that it did.
func (w *Worker) bury(ctx context.Context, j storedb.ClaimJobRow, why string) {
	if w.deadLetter(ctx, j, why) {
		w.log.ErrorContext(ctx, "job given up: it will not be tried again", "kind", j.Kind, "job_id", j.ID,
			"attempt", j.Attempts, "error", lastError(why).String)
	}
}

// deadLetter moves j to the dead letter with why as its last error, and
// reports whether j's lease was still its own to do so.
func (w *Worker) deadLetter(ctx context.Context, j storedb.ClaimJobRow, why string) bool {
	return w.settle(ctx, j, func(q *storedb.Queries) (int64, error) {
		return q.DeadLetterJob(ctx, storedb.DeadLetterJobParams{
			LastError:  lastError(why),
			FinishedAt: w.now(),
			ID:         j.ID,
			LeaseToken: j.LeaseToken,
		})
	})
}

// now returns the Worker's clock as a timestamp for the database.
func (w *Worker) now() sql.NullInt64 {
	return sql.NullInt64{Int64: w.clock().UnixMilli(), Valid: true}
}

// settle runs update, which records j's outcome if j's lease is still its
// own and returns the rows it changed, in one transaction, so that update
// reads the clock once the write lock is held. It reports
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
		w.log.WarnContext(ctx, "job outcome not recorded: its time limit ran out, so it may run again", "kind", j.Kind,
			"job_id", j.ID, "attempt", j.Attempts)
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
		w.log.WarnContext(ctx, "random source unreadable, so the next wait of a job has no random spread", "error", err)
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
