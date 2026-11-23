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

// Invalid is an input that a rule refuses. Field names the input, as the
// form or API names it, and Reason says which rule it breaks. Both are
// set by the code, never from the input: neither repeats the value given,
// which may be a password.
type Invalid struct {
	Field, Reason string
}

// Conflict is a change that the current state refuses, such as a name
// already taken.
type Conflict struct{}

// RateLimited is a request refused for coming too often. RetryAfter is how
// long the caller should wait before trying again: zero or more.
type RateLimited struct {
	RetryAfter time.Duration
}

func (*NotFound) Error() string        { return "app: not found" }
func (*Forbidden) Error() string       { return "app: forbidden" }
func (*Unauthenticated) Error() string { return "app: not signed in" }
func (*Conflict) Error() string        { return "app: conflict" }

func (e *Invalid) Error() string {
	if e == nil {
		return "app: invalid"
	}
	return "app: invalid " + e.Field + ": " + e.Reason
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
