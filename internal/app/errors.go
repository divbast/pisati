// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package app holds Pisati's use cases: the shell that does the I/O the
// core's rules decide on. Sessions keeps who is signed in, in the
// database, behind a cookie; Hasher hashes and verifies passwords, one at
// a time.
//
// Every use case returns an error as error. When the caller can act on
// it, it is, or wraps, an Error, which a transport finds with
// errors.AsType[app.Error] and maps once, with a switch over every
// variant. Any other error is the server's fault.
package app

import (
	"fmt"
	"time"
)

// Error is a failure the caller caused or can act on: NotFound,
// Forbidden, Unauthenticated, Invalid, Conflict or RateLimited. Each is a
// pointer type, so a value of the struct alone is no Error. Its message
// never holds a secret or personal data, and so may be logged.
//
//sumtype:decl
type Error interface {
	error
	appError()
}

// NotFound is a request for something that does not exist, or that the
// caller may not know exists.
type NotFound struct{}

// Forbidden is a request the caller is signed in for, but may not make.
type Forbidden struct{}

// Unauthenticated is a request that needs the caller to be signed in, when
// they are not.
type Unauthenticated struct{}

// Invalid is an input that a rule refuses: Field names the input, and
// Reason the rule it breaks. Neither is built from the input, so neither
// repeats the value given, which may be a password.
type Invalid struct {
	Field  Field
	Reason Reason
}

// Field names an input as the form or API that takes it names it, as in
// title or slug. Callers declare the names of their fields as constants of
// it, and never build one from what a request holds.
type Field string

// Reason is the rule an input breaks: one of the constants below, which a
// transport maps each to a message of its own. It is a number, so it can
// carry no text of the input's. The zero Reason is unspecified. The set
// starts with the rules every form needs, and grows with the rules later
// use cases bring.
type Reason uint8

const (
	// Required is an input that is missing or empty.
	Required Reason = iota + 1
	// TooShort is an input shorter than its field allows.
	TooShort
	// TooLong is an input longer than its field allows.
	TooLong
	// NotAllowed is an input of the right form that its field does not
	// accept, such as a name already reserved, or a password that holds
	// the user's name.
	NotAllowed
	// Malformed is an input not of the form its field takes, such as a
	// slug with a space in it, or a date that is no date.
	Malformed
)

// String returns the rule's name, for logs: required, too short, too
// long, not allowed or malformed, and unspecified for the zero Reason or
// one outside the set.
func (r Reason) String() string {
	switch r {
	case Required:
		return "required"
	case TooShort:
		return "too short"
	case TooLong:
		return "too long"
	case NotAllowed:
		return "not allowed"
	case Malformed:
		return "malformed"
	}
	return "unspecified"
}

// Conflict is a change that the current state refuses, such as a name
// already taken.
type Conflict struct{}

// RateLimited is a request refused for coming too often. RetryAfter is how
// long the caller should wait before trying again, and RetryAfterSeconds
// gives it as a Retry-After header takes it.
type RateLimited struct {
	RetryAfter time.Duration
}

// RetryAfterSeconds returns RetryAfter in whole seconds, rounded up, so
// that a client that waits that long has waited long enough: 0 when
// RetryAfter is zero or negative, and for a nil RateLimited.
func (e *RateLimited) RetryAfterSeconds() int64 {
	if e == nil || e.RetryAfter <= 0 {
		return 0
	}
	// Rounded up by the remainder, not by adding a second less a
	// nanosecond, which would overflow the longest Duration.
	seconds := int64(e.RetryAfter / time.Second)
	if e.RetryAfter%time.Second != 0 {
		seconds++
	}
	return seconds
}

func (*NotFound) Error() string        { return "app: not found" }
func (*Forbidden) Error() string       { return "app: forbidden" }
func (*Unauthenticated) Error() string { return "app: not signed in" }
func (*Conflict) Error() string        { return "app: conflict" }

func (e *Invalid) Error() string {
	if e == nil {
		return "app: invalid"
	}
	return "app: invalid " + string(e.Field) + ": " + e.Reason.String()
}

func (e *RateLimited) Error() string {
	if e == nil {
		return "app: rate limited"
	}
	return fmt.Sprintf("app: rate limited; retry after %s", e.RetryAfter)
}

// Each variant is an Error as a pointer, and only as one: the compiler
// holds every variant to that here, and refuses a value of one wherever
// an Error is wanted.
var (
	_ Error = (*NotFound)(nil)
	_ Error = (*Forbidden)(nil)
	_ Error = (*Unauthenticated)(nil)
	_ Error = (*Invalid)(nil)
	_ Error = (*Conflict)(nil)
	_ Error = (*RateLimited)(nil)
)

func (*NotFound) appError()        {}
func (*Forbidden) appError()       {}
func (*Unauthenticated) appError() {}
func (*Invalid) appError()         {}
func (*Conflict) appError()        {}
func (*RateLimited) appError()     {}
