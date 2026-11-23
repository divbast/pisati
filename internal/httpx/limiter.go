// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// ErrInvalid is a value its rule refuses: options given to NewLimiter or
// NewGuardedClient. Every error the package returns for one wraps it and
// says which rule.
var ErrInvalid = errors.New("invalid")

// Rate is how many events happen in a second.
type Rate float64

// LimiterOptions are how a Limiter limits. NewLimiter refuses options with
// any field outside its range, the zero LimiterOptions included.
// PageLimiterOptions and CSPReportLimiterOptions return the values Pisati
// limits with.
type LimiterOptions struct {
	// Rate is how fast a client's bucket refills, in requests a second.
	// Positive, and at most one a nanosecond.
	Rate Rate
	// Burst is how many requests a full bucket holds: how many a client
	// that has rested may make at once. At least 1.
	Burst int
	// MaxEntries is how many clients the Limiter keeps a bucket for at
	// most. At least 1.
	MaxEntries int
	// Idle is how long a client goes unseen before the Limiter forgets
	// it. At least Burst / Rate, how long an empty bucket takes to fill,
	// so that a client is only forgotten once its bucket is full again.
	Idle time.Duration
}

// PageLimiterOptions returns the LimiterOptions Pisati limits its pages
// with: 10 requests a second, in bursts of up to 40, for up to 65,536
// clients, each forgotten after 10 minutes unseen.
func PageLimiterOptions() LimiterOptions {
	return LimiterOptions{Rate: 10, Burst: 40, MaxEntries: 1 << 16, Idle: 10 * time.Minute}
}

// CSPReportLimiterOptions returns the LimiterOptions Pisati limits CSP
// violation reports with: 1 a second, in bursts of up to 5, for up to
// 65,536 clients, each forgotten after 10 minutes unseen.
func CSPReportLimiterOptions() LimiterOptions {
	return LimiterOptions{Rate: 1, Burst: 5, MaxEntries: 1 << 16, Idle: 10 * time.Minute}
}

// perToken returns how many nanoseconds a bucket filling at r takes to
// gain one token.
func perToken(r Rate) float64 {
	return float64(time.Second) / float64(r)
}

// problems returns an error for each field of o outside its range.
func (o LimiterOptions) problems() []error {
	var errs []error
	// NaN fails both comparisons, and +Inf the second.
	rateOK := o.Rate > 0 && o.Rate <= Rate(time.Second)
	if !rateOK {
		errs = append(errs, fmt.Errorf("httpx: %w limiter options: Rate is %v, want it positive and at most one a nanosecond",
			ErrInvalid, o.Rate))
	}
	if o.Burst < 1 {
		errs = append(errs, fmt.Errorf("httpx: %w limiter options: Burst is %d, want at least 1", ErrInvalid, o.Burst))
	}
	if o.MaxEntries < 1 {
		errs = append(errs, fmt.Errorf("httpx: %w limiter options: MaxEntries is %d, want at least 1", ErrInvalid, o.MaxEntries))
	}
	switch {
	case o.Idle <= 0:
		errs = append(errs, fmt.Errorf("httpx: %w limiter options: Idle is %v, want it positive", ErrInvalid, o.Idle))
	case rateOK && o.Burst >= 1:
		// Compared first in floating point, where a slow rate cannot
		// overflow, then exactly, as NewLimiter will round the interval.
		per := perToken(o.Rate)
		if per > float64(o.Idle) || time.Duration(math.Round(per)) > o.Idle/time.Duration(o.Burst) {
			errs = append(errs, fmt.Errorf("httpx: %w limiter options: Idle is %v, shorter than Burst / Rate, "+
				"the time an empty bucket takes to fill", ErrInvalid, o.Idle))
		}
	}
	return errs
}

// Limiter limits how often each client is served. Each client has a
// token bucket that holds LimiterOptions.Burst tokens and refills at
// Rate: a request takes a token, and one that finds the bucket empty is
// refused. Clients are told apart by address: an IPv4 client by its own,
// and an IPv6 client by its /64, as a client can move freely within its
// /64. Requests with no client address share one bucket. Make one with
// NewLimiter. The zero Limiter limits nothing: its Middleware is a bug,
// and panics.
//
// The table of buckets is bounded, and starts no goroutine to keep it
// so. It holds at most MaxEntries clients. A client unseen for Idle is
// forgotten, which costs it nothing, as its bucket is full by then: each
// request forgets up to 64 such clients, those seen longest ago first. A
// new client that finds the table full of clients seen within Idle takes
// the place of the one seen longest ago, whose bucket is full again if it
// returns, so that new clients are never all refused.
type Limiter struct {
	// interval is how long a bucket takes to gain one token, and depth
	// how long it takes to fill: interval times Burst.
	interval, depth time.Duration
	idle            time.Duration
	maxEntries      int
	clock           func() time.Time

	mu      sync.Mutex
	buckets map[netip.Prefix]*bucket
	// newest and oldest end the list of buckets in the order their
	// clients were last seen.
	newest, oldest *bucket
}

// bucket is one client's token bucket, kept as the time it is full
// again. A request moves that time an interval later, and is refused
// when that would put it more than the Limiter's depth from now. A whole
// number of nanoseconds, it never drifts as a count of tokens would.
type bucket struct {
	key          netip.Prefix
	full, seen   time.Time
	newer, older *bucket
}

// NewLimiter returns a Limiter that limits by o, reading the time from
// clock. The Limiter calls clock from each request's goroutine, so it
// must be safe for concurrent use, as time.Now is. A clock that goes back
// is taken, for each bucket, as standing still.
//
// NewLimiter returns an error wrapping ErrInvalid, naming every problem
// at once, when clock is nil or o has a field outside its range.
func NewLimiter(o LimiterOptions, clock func() time.Time) (*Limiter, error) {
	errs := o.problems()
	if clock == nil {
		errs = append(errs, fmt.Errorf("httpx: %w limiter clock: nil", ErrInvalid))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	interval := time.Duration(math.Round(perToken(o.Rate)))
	return &Limiter{
		interval:   interval,
		depth:      interval * time.Duration(o.Burst),
		idle:       o.Idle,
		maxEntries: o.MaxEntries,
		clock:      clock,
		buckets:    make(map[netip.Prefix]*bucket),
	}, nil
}

// Middleware returns the middleware that serves a request only when its
// client, as ip finds it, has a token to spend. A request refused gets
// 429 with a generic body, and a Retry-After of the whole seconds,
// rounded up, until its client's next token, and never reaches the
// handler. A nil ip, or a Limiter not made by NewLimiter, is a bug, and
// Middleware panics on it.
func (l *Limiter) Middleware(ip ClientIP) Middleware {
	if l == nil || l.buckets == nil {
		panic("httpx: Middleware on a Limiter not made by NewLimiter")
	}
	if ip == nil {
		panic("httpx: Limiter.Middleware needs a ClientIP")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var key netip.Prefix
			if a, ok := ip(r); ok {
				key = keyOf(a)
			}
			if wait, ok := l.take(key); !ok {
				w.Header().Set("Retry-After", strconv.FormatInt(wholeSeconds(wait), 10))
				http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Len returns how many clients l keeps a bucket for: never more than
// LimiterOptions.MaxEntries. The zero Limiter keeps none.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// sweepBatch bounds how many idle clients one request forgets, so that no
// request pays for a long quiet spell at once.
const sweepBatch = 64

// take spends a token from key's bucket, and reports whether there was
// one. When there was not, it returns how long until there is.
func (l *Limiter) take(key netip.Prefix) (time.Duration, bool) {
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b, ok := l.buckets[key]
	if ok {
		l.unlink(b)
		now = later(now, b.seen)
	} else {
		if len(l.buckets) >= l.maxEntries {
			l.remove(l.oldest)
		}
		b = &bucket{key: key, full: now}
		l.buckets[key] = b
	}
	b.seen = now
	l.pushNewest(b)
	next := later(b.full, now).Add(l.interval)
	if wait := next.Sub(now) - l.depth; wait > 0 {
		return wait, false
	}
	b.full = next
	return 0, true
}

// sweep forgets up to sweepBatch of the clients unseen for l.idle by now,
// those seen longest ago first.
func (l *Limiter) sweep(now time.Time) {
	for range sweepBatch {
		b := l.oldest
		if b == nil || now.Sub(b.seen) < l.idle {
			return
		}
		l.remove(b)
	}
}

// remove forgets b's client.
func (l *Limiter) remove(b *bucket) {
	l.unlink(b)
	delete(l.buckets, b.key)
}

// unlink takes b out of the list of buckets.
func (l *Limiter) unlink(b *bucket) {
	if b.newer != nil {
		b.newer.older = b.older
	} else {
		l.newest = b.older
	}
	if b.older != nil {
		b.older.newer = b.newer
	} else {
		l.oldest = b.newer
	}
	b.newer, b.older = nil, nil
}

// pushNewest puts b at the newest end of the list of buckets.
func (l *Limiter) pushNewest(b *bucket) {
	b.older = l.newest
	if l.newest != nil {
		l.newest.newer = b
	}
	l.newest = b
	if l.oldest == nil {
		l.oldest = b
	}
}

// keyOf returns the key of the bucket of the client at a: its /64 for an
// IPv6 address, and the address itself for an IPv4 one, however it is
// spelt.
func keyOf(a netip.Addr) netip.Prefix {
	a = plain(a)
	if a.Is4() {
		return netip.PrefixFrom(a, 32)
	}
	return netip.PrefixFrom(a, 64).Masked()
}

// later returns the later of a and b.
func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// wholeSeconds returns d, which is positive, in whole seconds, rounded
// up.
func wholeSeconds(d time.Duration) int64 {
	s := int64(d / time.Second)
	if d%time.Second != 0 {
		s++
	}
	return s
}
