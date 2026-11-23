// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs_test

import (
	"math"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/jobs"
)

// The schedule DefaultOptions approves: 30 s, doubling to 6 h, ±20%.
const (
	base    = 30 * time.Second
	backCap = 6 * time.Hour
	ceiling = backCap * 6 / 5 // 7 h 12 min: the cap, jittered up
	floor   = base * 4 / 5    // 24 s: the base, jittered down
)

func TestDefaultOptions(t *testing.T) {
	want := jobs.Options{
		Workers:     4,
		Lease:       5 * time.Minute,
		BackoffBase: base,
		BackoffCap:  backCap,
		Jitter:      0.2,
		MaxAttempts: 12,
		Poll:        time.Second,
		Drain:       8 * time.Second,
		SweepEvery:  time.Hour,
		KeepDone:    7 * 24 * time.Hour,
		KeepDead:    30 * 24 * time.Hour,
	}
	if got := jobs.DefaultOptions(); got != want {
		t.Errorf("DefaultOptions() = %+v, want the approved values %+v", got, want)
	}
}

func TestBackoffBounds(t *testing.T) {
	for _, c := range []struct {
		attempt int
		want    time.Duration // before jitter
	}{
		{math.MinInt, base},
		{-1, base},
		{0, base},
		{1, base},
		{2, 2 * base},
		{3, 4 * base},
		{9, 256 * base},
		{10, 512 * base}, // 4 h 16 min, the last below the cap
		{11, backCap},
		{12, backCap},
		{100, backCap},
		{math.MaxInt, backCap},
	} {
		if got := jobs.Backoff(c.attempt, nil); got != c.want {
			t.Errorf("Backoff(%d, nil) = %v, want %v with no jitter", c.attempt, got, c.want)
		}
		for _, u := range []float64{math.NaN(), math.Inf(-1), -0.5, 1, 1.5, math.Inf(1)} {
			if got := jobs.Backoff(c.attempt, at(u)); got != c.want {
				t.Errorf("Backoff(%d, %v) = %v, want %v: a number outside [0, 1) adds no jitter", c.attempt, u, got, c.want)
			}
		}
		if got := jobs.Backoff(c.attempt, at(0.5)); got != c.want {
			t.Errorf("Backoff(%d, 0.5) = %v, want %v: the middle adds no jitter", c.attempt, got, c.want)
		}
		lo, hi := c.want*4/5, c.want*6/5
		previous := time.Duration(0)
		for _, u := range []float64{0, 0.1, 0.25, 0.5, 0.75, 0.9, math.Nextafter(1, 0)} {
			got := jobs.Backoff(c.attempt, at(u))
			if got < lo || got > hi {
				t.Errorf("Backoff(%d, %v) = %v, want within ±20%% of %v: [%v, %v]", c.attempt, u, got, c.want, lo, hi)
			}
			if got > ceiling {
				t.Errorf("Backoff(%d, %v) = %v, above 6 h × 1.2", c.attempt, u, got)
			}
			if got < previous {
				t.Errorf("Backoff(%d, %v) = %v, less than %v for a smaller number", c.attempt, u, got, previous)
			}
			previous = got
		}
		// The extremes reach the full ±20%, to within rounding.
		for u, want := range map[float64]time.Duration{0: lo, math.Nextafter(1, 0): hi} {
			if got := jobs.Backoff(c.attempt, at(u)); (got - want).Abs() > time.Microsecond {
				t.Errorf("Backoff(%d, %v) = %v, want %v", c.attempt, u, got, want)
			}
		}
	}
}

func FuzzBackoff(f *testing.F) {
	for _, c := range []struct {
		attempt int
		u       float64
	}{
		{1, 0.5},
		{0, 0},
		{2, 0.999},
		{11, math.Nextafter(1, 0)},
		{math.MaxInt, 0.5},
		{math.MinInt, 0.25},
		{3, math.NaN()},
		{4, -1},
	} {
		f.Add(c.attempt, c.u)
	}
	f.Fuzz(func(t *testing.T, attempt int, u float64) {
		got := jobs.Backoff(attempt, at(u))
		if got < 0 {
			t.Fatalf("Backoff(%d, %v) = %v, negative", attempt, u, got)
		}
		if got < floor || got > ceiling {
			t.Fatalf("Backoff(%d, %v) = %v, outside [%v, %v]", attempt, u, got, floor, ceiling)
		}
		plain := jobs.Backoff(attempt, nil)
		if plain < base || plain > backCap {
			t.Fatalf("Backoff(%d, nil) = %v, outside [%v, %v]", attempt, plain, base, backCap)
		}
		if got < plain*4/5 || got > plain*6/5 {
			t.Fatalf("Backoff(%d, %v) = %v, more than 20%% from %v", attempt, u, got, plain)
		}
		if attempt < math.MaxInt {
			if next := jobs.Backoff(attempt+1, at(u)); next < got {
				t.Fatalf("Backoff(%d, %v) = %v, less than Backoff(%d) = %v", attempt+1, u, next, attempt, got)
			}
			if next := jobs.Backoff(attempt+1, nil); next < plain {
				t.Fatalf("Backoff(%d, nil) = %v, less than Backoff(%d, nil) = %v", attempt+1, next, attempt, plain)
			}
		}
	})
}
