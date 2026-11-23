// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package auth_test

import (
	"testing"
	"time"

	"github.com/divbast/pisati/internal/core/auth"
)

// The approved values, written out so that a change to a constant fails
// the tests.
const (
	day      = 24 * time.Hour
	idle     = 7 * day
	absolute = 30 * day
	touch    = time.Minute
	stepUp   = 10 * time.Minute
)

// now is the instant every case is judged at.
var now = time.Date(2026, time.November, 23, 9, 30, 0, 0, time.UTC)

func TestSessionLimitsAreTheApprovedValues(t *testing.T) {
	if auth.IdleLimit != idle || auth.AbsoluteLimit != absolute || auth.TouchInterval != touch || auth.StepUpWindow != stepUp {
		t.Errorf("limits idle %v, absolute %v, touch %v, step-up %v; want %v, %v, %v, %v",
			auth.IdleLimit, auth.AbsoluteLimit, auth.TouchInterval, auth.StepUpWindow, idle, absolute, touch, stepUp)
	}
}

func TestEvaluateSession(t *testing.T) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	sydney := time.FixedZone("AEDT", 11*60*60)
	for _, c := range []struct {
		name              string
		created, lastSeen time.Time
		want              auth.SessionState
	}{
		{"just created", now, now, auth.Live{TouchDue: false}},
		{"seen just under a minute ago", ago(time.Hour), ago(touch - time.Nanosecond), auth.Live{TouchDue: false}},
		{"seen a minute ago", ago(time.Hour), ago(touch), auth.Live{TouchDue: true}},
		{"seen an hour ago", ago(2 * time.Hour), ago(time.Hour), auth.Live{TouchDue: true}},
		{"idle just under 7 days", ago(8 * day), ago(idle - time.Nanosecond), auth.Live{TouchDue: true}},
		{"idle 7 days", ago(8 * day), ago(idle), auth.IdleExpired{}},
		{"idle 8 days", ago(9 * day), ago(8 * day), auth.IdleExpired{}},
		{"just under 30 days old, seen now", ago(absolute - time.Nanosecond), now, auth.Live{TouchDue: false}},
		{"just under 30 days old, seen an hour ago", ago(absolute - time.Nanosecond), ago(time.Hour), auth.Live{TouchDue: true}},
		{"30 days old, seen now", ago(absolute), now, auth.AbsoluteExpired{}},
		{"31 days old, seen an hour ago", ago(31 * day), ago(time.Hour), auth.AbsoluteExpired{}},
		{"past both limits: the absolute one is reported", ago(40 * day), ago(idle), auth.AbsoluteExpired{}},
		{"the same instants in another zone", ago(time.Hour).In(sydney), ago(touch).In(sydney), auth.Live{TouchDue: true}},
		{"judged in another zone", ago(absolute).In(sydney), ago(time.Hour).In(sydney), auth.AbsoluteExpired{}},
		// The clock has moved back: a time after now counts as no time
		// since, so no touch is due, which would move LastSeen backwards.
		{"seen a second from now", ago(time.Hour), now.Add(time.Second), auth.Live{TouchDue: false}},
		{"created and seen a second from now", now.Add(time.Second), now.Add(time.Second), auth.Live{TouchDue: false}},
		{"created a day from now, seen an hour ago", now.Add(day), ago(time.Hour), auth.Live{TouchDue: false}},
		// A time after now does not keep alive a session that the other
		// time has expired.
		{"31 days old, seen a second from now", ago(31 * day), now.Add(time.Second), auth.AbsoluteExpired{}},
		{"created a day from now, idle 8 days", now.Add(day), ago(8 * day), auth.IdleExpired{}},
		// The zero time is long past every limit.
		{"the zero SessionTimes", time.Time{}, time.Time{}, auth.AbsoluteExpired{}},
		{"never seen", ago(time.Hour), time.Time{}, auth.IdleExpired{}},
	} {
		got := auth.Evaluate(auth.SessionTimes{Created: c.created, LastSeen: c.lastSeen}, now)
		if got != c.want {
			t.Errorf("%s: Evaluate = %s, want %s", c.name, state(got), state(c.want))
		}
	}
}

// state names s, listing every variant so that a new one fails the build
// until it is named here too.
func state(s auth.SessionState) string {
	switch s := s.(type) {
	case auth.Live:
		if s.TouchDue {
			return "Live{TouchDue: true}"
		}
		return "Live{TouchDue: false}"
	case auth.IdleExpired:
		return "IdleExpired"
	case auth.AbsoluteExpired:
		return "AbsoluteExpired"
	case nil:
		return "nil"
	default:
		panic("unknown SessionState")
	}
}

func TestStepUpFresh(t *testing.T) {
	for _, c := range []struct {
		name          string
		lastAuth, now time.Time
		want          bool
	}{
		{"signed in now", now, now, true},
		{"a minute ago", now.Add(-time.Minute), now, true},
		{"just under 10 minutes ago", now.Add(-stepUp + time.Nanosecond), now, true},
		{"10 minutes ago", now.Add(-stepUp), now, false},
		{"an hour ago", now.Add(-time.Hour), now, false},
		{"a nanosecond from now", now.Add(time.Nanosecond), now, false},
		{"a day from now", now.Add(day), now, false},
		{"never", time.Time{}, now, false},
		{"never, judged 5 minutes after the zero time", time.Time{}, time.Time{}.Add(5 * time.Minute), false},
		{"never, judged at the zero time", time.Time{}, time.Time{}, false},
		{"in another zone", now.Add(-time.Minute).In(time.FixedZone("AEDT", 11*60*60)), now, true},
	} {
		if got := auth.StepUpFresh(c.lastAuth, c.now); got != c.want {
			t.Errorf("%s: StepUpFresh(%v, %v) = %v, want %v", c.name, c.lastAuth, c.now, got, c.want)
		}
	}
}
