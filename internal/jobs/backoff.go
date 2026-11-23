// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs

import (
	"math"
	"time"
)

// Backoff returns how long a job waits to be tried again after its
// attempt-th attempt fails, under DefaultOptions: 30 s after the first,
// doubling after each attempt to at most 6 h, then moved by up to 20%
// either way, as rnd says. An attempt below 1 counts as the first.
//
// rnd returns a number in [0, 1): 0 shortens the wait by 20%, 0.5 leaves
// it as it is, and numbers towards 1 lengthen it by up to 20%. A nil rnd,
// or one that returns a number outside [0, 1), adds no jitter. So the
// result is never below 24 s nor above 7 h 12 min, and for a given number
// from rnd it never falls as attempt grows.
func Backoff(attempt int, rnd func() float64) time.Duration {
	return DefaultOptions().backoff(int64(attempt), rnd)
}

// backoff is Backoff under o's schedule: o.BackoffBase after the first
// attempt, doubling to o.BackoffCap, moved by up to o.Jitter either way.
func (o Options) backoff(attempt int64, rnd func() float64) time.Duration {
	d := o.BackoffBase
	for n := int64(1); n < attempt && d < o.BackoffCap; n++ {
		// Doubling past half the cap could overflow; the cap is the answer.
		if d > o.BackoffCap/2 {
			d = o.BackoffCap
			break
		}
		d *= 2
	}
	d = min(d, o.BackoffCap)
	if rnd == nil {
		return d
	}
	u := rnd()
	if math.IsNaN(u) || u < 0 || u >= 1 {
		return d
	}
	jittered := float64(d) * (1 + o.Jitter*(2*u-1))
	if jittered >= math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(jittered)
}
