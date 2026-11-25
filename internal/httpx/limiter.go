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
// NewGuardedClient, a request GuardedClient.Do will not send as it is, or
// a response it will not return as it came, as its body is still encoded.
// Every error the package returns for one wraps it and says which rule.
var ErrInvalid = errors.New("invalid")

// Rate is how many events happen in a second.
type Rate float64

// networkFactor is how many times as many tokens an IPv6 /48's bucket
// holds, and how many times as fast it refills, as a client's.
const networkFactor = 8

// maxRate is the fastest Rate a Limiter takes: one at which even a /48's
// bucket gains at most one token a nanosecond.
const maxRate = Rate(time.Second) / networkFactor

// LimiterOptions are how a Limiter limits. NewLimiter refuses options with
// any field outside its range, the zero LimiterOptions included.
// PageLimiterOptions and CSPReportLimiterOptions return the values Pisati
// limits with.
type LimiterOptions struct {
	// Rate is how fast a client's bucket refills, in requests a second.
	// Positive, and at most 125,000,000 a second, so that a /48's bucket,
	// 8 times as fast, gains at most one token a nanosecond.
	Rate Rate
	// Burst is how many requests a client's full bucket holds: how many a
	// client that has rested may make at once. At least 1.
	Burst int
	// MaxEntries is how many buckets the Limiter keeps at most: one for
	// each IPv4 address, and one for each IPv6 /64 and each IPv6 /48 that
	// requests come from. At least 2, room for an IPv6 client's two.
	MaxEntries int
	// Idle is how long a bucket goes unused before the Limiter forgets it.
	// At least Burst / Rate, how long an empty bucket takes to fill, so
	// that a bucket is only forgotten once it is full again.
	Idle time.Duration
}

// PageLimiterOptions returns the LimiterOptions Pisati limits its pages
// with: 10 requests a second, in bursts of up to 40, in up to 65,536
// buckets, each forgotten after 10 minutes unused.
func PageLimiterOptions() LimiterOptions {
	return LimiterOptions{Rate: 10, Burst: 40, MaxEntries: 1 << 16, Idle: 10 * time.Minute}
}

// CSPReportLimiterOptions returns the LimiterOptions Pisati limits CSP
// violation reports with: 1 a second, in bursts of up to 5, in up to
// 65,536 buckets, each forgotten after 10 minutes unused.
func CSPReportLimiterOptions() LimiterOptions {
	return LimiterOptions{Rate: 1, Burst: 5, MaxEntries: 1 << 16, Idle: 10 * time.Minute}
}

// rule is how one kind of bucket fills: a token each interval, to as many
// as depth holds.
type rule struct {
	interval, depth time.Duration
}

// newRule returns the rule of a bucket that holds factor times burst
// tokens and gains factor times rate of them a second, and whether a
// Duration can hold it with the bucket filling within idle. rate, burst
// and idle are positive, and rate at most maxRate.
func newRule(rate Rate, burst, factor int, idle time.Duration) (rule, bool) {
	per := float64(time.Second) / (float64(rate) * float64(factor))
	// float64(math.MaxInt64) is 2⁶³, which no Duration holds, and which
	// a conversion would wrap or saturate.
	if per >= float64(math.MaxInt64) {
		return rule{}, false
	}
	interval := time.Duration(math.Round(per))
	// Dividing, rather than multiplying, cannot overflow.
	if interval < 1 || interval > idle/time.Duration(burst)/time.Duration(factor) {
		return rule{}, false
	}
	return rule{interval: interval, depth: interval * time.Duration(burst) * time.Duration(factor)}, true
}

// problems returns an error for each field of o outside its range.
func (o LimiterOptions) problems() []error {
	var errs []error
	// NaN fails both comparisons, and +Inf the second.
	rateOK := o.Rate > 0 && o.Rate <= maxRate
	if !rateOK {
		errs = append(errs, fmt.Errorf("httpx: %w limiter options: Rate is %v, want it positive and at most %v",
			ErrInvalid, o.Rate, maxRate))
	}
	if o.Burst < 1 {
		errs = append(errs, fmt.Errorf("httpx: %w limiter options: Burst is %d, want at least 1", ErrInvalid, o.Burst))
	}
	if o.MaxEntries < 2 {
		errs = append(errs, fmt.Errorf("httpx: %w limiter options: MaxEntries is %d, want at least 2, "+
			"room for an IPv6 client's two buckets", ErrInvalid, o.MaxEntries))
	}
	switch {
	case o.Idle <= 0:
		errs = append(errs, fmt.Errorf("httpx: %w limiter options: Idle is %v, want it positive", ErrInvalid, o.Idle))
	case rateOK && o.Burst >= 1:
		_, client := newRule(o.Rate, o.Burst, 1, o.Idle)
		_, network := newRule(o.Rate, o.Burst, networkFactor, o.Idle)
		if !client || !network {
			errs = append(errs, fmt.Errorf("httpx: %w limiter options: Idle is %v, shorter than Burst / Rate, "+
				"the time an empty bucket takes to fill", ErrInvalid, o.Idle))
		}
	}
	return errs
}

// Limiter limits how often each client is served, with token buckets: a
// request takes a token from each of its client's buckets, and one that
// finds any of them empty is refused. An IPv4 client has one bucket, its
// address's, which holds LimiterOptions.Burst tokens and refills at Rate.
// An IPv6 client has two. Its /64's bucket is like an IPv4 address's,
// as a client can move freely within its /64. Its /48's, which every /64
// in the /48 shares, holds and refills 8 times as many, so that one
// holder of a /48 cannot spend a burst for each of its /64s. Requests
// with no client address, or an invalid one, share one bucket. A refused
// request makes no bucket, so that a client refused cannot crowd others
// out of the table with buckets for addresses it never had served. Make
// a Limiter with NewLimiter. The zero Limiter limits nothing: its
// Middleware is a bug, and panics.
//
// The table of buckets is bounded, and starts no goroutine to keep it
// so. It holds at most MaxEntries buckets, of both kinds. A bucket unused
// for Idle is forgotten, which costs its client nothing, as it is full by
// then: each request forgets up to 64 such buckets, those used longest
// ago first. A new bucket that finds the table full of buckets used
// within Idle takes the place of the one used longest ago, which is full
// again if its client returns, so that new clients are never all
// refused.
type Limiter struct {
	// client is how a bucket of an IPv4 address or an IPv6 /64 fills, and
	// network how an IPv6 /48's does.
	client, network rule
	idle            time.Duration
	maxEntries      int
	clock           func() time.Time

	mu sync.Mutex
	// now is the Limiter's own time, which moves on as far as the clock
	// has moved forward since read, its last reading, and never goes back.
	// ticked is whether the clock has been read yet.
	read, now time.Time
	ticked    bool
	buckets   map[netip.Prefix]*bucket
	// newest and oldest end the list of buckets in the order they were
	// last used, which is also the order of their seen times.
	newest, oldest *bucket
}

// bucket is one token bucket, kept as the time it is full again. A
// request moves that time an interval later, and is refused when that
// would put it more than the rule's depth from now. A whole number of
// nanoseconds, it never drifts as a count of tokens would.
type bucket struct {
	key          netip.Prefix
	full, seen   time.Time
	newer, older *bucket
}

// claim is a bucket a request needs a token from, and how it fills.
type claim struct {
	key  netip.Prefix
	rule rule
}

// NewLimiter returns a Limiter that limits by o, reading the time from
// clock. The Limiter reads clock once for each request, from the
// request's goroutine, so it must be safe for concurrent use, as time.Now
// is. The Limiter's own time moves on as far as clock moves forward: a
// clock that goes back hands out no token, and from where it went back
// to, time counts again.
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
	// problems has checked that both rules hold.
	client, _ := newRule(o.Rate, o.Burst, 1, o.Idle)
	network, _ := newRule(o.Rate, o.Burst, networkFactor, o.Idle)
	return &Limiter{
		client:     client,
		network:    network,
		idle:       o.Idle,
		maxEntries: o.MaxEntries,
		clock:      clock,
		buckets:    make(map[netip.Prefix]*bucket),
	}, nil
}

// Middleware returns the middleware that serves a request only when each
// of its client's buckets, as ip finds the client, has a token to spend.
// A request refused gets 429 with a generic body, Cache-Control:
// no-store, as the refusal answers that client alone, and a Retry-After
// of the whole seconds, rounded up, until every bucket that refused it
// has a token again, and never reaches the handler. A refused request takes
// no token from any bucket. A nil ip, or a Limiter not made by
// NewLimiter, is a bug, and Middleware panics on it.
func (l *Limiter) Middleware(ip ClientIP) Middleware {
	if l == nil || l.buckets == nil {
		panic("httpx: Middleware on a Limiter not made by NewLimiter")
	}
	if ip == nil {
		panic("httpx: Limiter.Middleware needs a ClientIP")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if wait, ok := l.admit(r, ip); !ok {
				w.Header().Set("Retry-After", strconv.FormatInt(wholeSeconds(wait), 10))
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Len returns how many buckets l keeps: never more than
// LimiterOptions.MaxEntries. The zero Limiter keeps none.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// admit takes a token from each bucket of r's client, as ip finds it, and
// reports whether each had one. When one did not, it returns how long
// until all do.
func (l *Limiter) admit(r *http.Request, ip ClientIP) (time.Duration, bool) {
	a, ok := ip(r)
	a = plain(a)
	switch {
	case !ok || !a.IsValid():
		return l.take(claim{rule: l.client})
	case a.Is4():
		return l.take(claim{key: netip.PrefixFrom(a, 32), rule: l.client})
	default:
		return l.take(
			claim{key: netip.PrefixFrom(a, 64).Masked(), rule: l.client},
			claim{key: netip.PrefixFrom(a, 48).Masked(), rule: l.network},
		)
	}
}

// sweepBatch bounds how many idle buckets one request forgets, so that no
// request pays for a long quiet spell at once.
const sweepBatch = 64

// take spends a token from the bucket of each claim, when every one has
// a token, and reports whether they did. When one did not, it takes none,
// and returns how long until every one has a token. A claim with no
// bucket yet has a full one, which is made only when the request is let
// through.
func (l *Limiter) take(claims ...claim) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.tick()
	l.sweep(now)
	held := make([]*bucket, len(claims))
	var wait time.Duration
	for i, c := range claims {
		full := now
		if b, ok := l.buckets[c.key]; ok {
			held[i] = b
			full = later(b.full, now)
		}
		wait = max(wait, full.Add(c.rule.interval).Sub(now)-c.rule.depth)
	}
	// Buckets that exist are used, refused or not, so that a busy
	// client's are kept, and the list stays in the order of seen.
	for _, b := range held {
		if b != nil {
			l.use(b, now)
		}
	}
	if wait > 0 {
		return wait, false
	}
	for i, c := range claims {
		if held[i] == nil {
			// Every bucket held was just used, so this evicts none of
			// them: MaxEntries leaves room for two.
			held[i] = l.add(c.key, now)
		}
		held[i].full = later(held[i].full, now).Add(c.rule.interval)
	}
	return 0, true
}

// tick reads the clock, and returns the Limiter's time: moved on as far as
// the clock has moved forward since its last reading, if it has. It is
// called with l.mu held, so that readings are ordered as the requests
// that make them.
func (l *Limiter) tick() time.Time {
	c := l.clock()
	if !l.ticked {
		l.read, l.now, l.ticked = c, c, true
		return c
	}
	if d := c.Sub(l.read); d > 0 {
		l.now = l.now.Add(d)
	}
	l.read = c
	return l.now
}

// use makes b the bucket used last, at now.
func (l *Limiter) use(b *bucket, now time.Time) {
	l.unlink(b)
	b.seen = now
	l.pushNewest(b)
}

// add returns a new, full bucket for key, used at now, forgetting the
// bucket used longest ago when the table is full.
func (l *Limiter) add(key netip.Prefix, now time.Time) *bucket {
	if len(l.buckets) >= l.maxEntries {
		l.remove(l.oldest)
	}
	b := &bucket{key: key, full: now, seen: now}
	l.buckets[key] = b
	l.pushNewest(b)
	return b
}

// sweep forgets up to sweepBatch of the buckets unused for l.idle by now,
// those used longest ago first.
func (l *Limiter) sweep(now time.Time) {
	for range sweepBatch {
		b := l.oldest
		if b == nil || now.Sub(b.seen) < l.idle {
			return
		}
		l.remove(b)
	}
}

// remove forgets b.
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
