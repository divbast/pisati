// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package auth

import "time"

// UserID names a user. IDs are positive, and the zero UserID names no
// user: a function that may find no user returns (UserID, bool), never a
// zero UserID that stands for none.
type UserID int64

// The session lifetimes and the step-up window. A limit is reached when
// the time elapsed is equal to it or longer.
const (
	// IdleLimit is how long a session lives without being used.
	IdleLimit = 7 * 24 * time.Hour
	// AbsoluteLimit is how long a session lives at most, however much it
	// is used.
	AbsoluteLimit = 30 * 24 * time.Hour
	// TouchInterval is how often a session's LastSeen is written, at most:
	// once a minute, rather than on every request.
	TouchInterval = time.Minute
	// StepUpWindow is how recent a sign-in must be for a sensitive action:
	// changing a password, MFA, an e-mail address or tokens, deleting, or
	// changing the site's configuration.
	StepUpWindow = 10 * time.Minute
)

// SessionTimes are the times a session's state follows from.
type SessionTimes struct {
	// Created is when the user signed in, and LastSeen when the session
	// was last touched (see Live).
	Created, LastSeen time.Time
}

// SessionState is what Evaluate decides: Live, IdleExpired or
// AbsoluteExpired.
//
//sumtype:decl
type SessionState interface {
	sessionState()
}

// Live is a session that may be used. TouchDue is true when LastSeen is
// TouchInterval or more in the past: the caller should then set LastSeen
// to now.
type Live struct {
	TouchDue bool
}

// IdleExpired is a session that has gone unused for IdleLimit.
type IdleExpired struct{}

// AbsoluteExpired is a session that is AbsoluteLimit old.
type AbsoluteExpired struct{}

func (Live) sessionState()            {}
func (IdleExpired) sessionState()     {}
func (AbsoluteExpired) sessionState() {}

// Evaluate returns the state at now of a session with times t:
// AbsoluteExpired once AbsoluteLimit has passed since Created, otherwise
// IdleExpired once IdleLimit has passed since LastSeen, and otherwise
// Live.
//
// A Created or LastSeen after now, as when the clock has moved back,
// counts as no time since it. Such a session is Live with no touch due,
// which would move LastSeen back, unless the other time has reached its
// limit: a time in the future never keeps alive a session that the other
// has expired. The zero time is long past both limits, so the zero
// SessionTimes is AbsoluteExpired.
func Evaluate(t SessionTimes, now time.Time) SessionState {
	if since(t.Created, now) >= AbsoluteLimit {
		return AbsoluteExpired{}
	}
	idle := since(t.LastSeen, now)
	if idle >= IdleLimit {
		return IdleExpired{}
	}
	if t.Created.After(now) || t.LastSeen.After(now) {
		return Live{TouchDue: false}
	}
	return Live{TouchDue: idle >= TouchInterval}
}

// since returns how long before now t was, or 0 when t is after now.
// time.Time.Sub saturates, so the zero time gives the longest Duration.
func since(t, now time.Time) time.Duration {
	return max(now.Sub(t), 0)
}

// StepUpFresh reports whether a sign-in at lastAuth is recent enough at
// now for a sensitive action: less than StepUpWindow before now. It fails
// closed: a zero lastAuth, which means no sign-in, and one after now, as
// when the clock has moved back, are not fresh.
func StepUpFresh(lastAuth, now time.Time) bool {
	if lastAuth.IsZero() || lastAuth.After(now) {
		return false
	}
	return now.Sub(lastAuth) < StepUpWindow
}
