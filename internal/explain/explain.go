// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package explain makes errors whose message is a sentence of Pisati's
// own, worded for the operator, around the error that caused it: the
// message says what happened in plain words, and errors.Is, errors.As
// and errors.AsType still find the cause, and everything the cause
// wraps, through it.
//
// Its errors exist so that a message can start with a capital letter, as
// "Pisati cannot …" does, and so that a sentinel's own word, such as
// "invalid", never shows: fmt.Errorf with %w would print the cause's
// message wherever the verb stands.
package explain

import "fmt"

// explained is an error whose message is msg, and which wraps cause, or
// nothing when cause is nil. It has Unwrap() error, never Unwrap()
// []error, so that it is one problem however many errors cause joins.
type explained struct {
	msg   string
	cause error
}

func (e *explained) Error() string { return e.msg }
func (e *explained) Unwrap() error { return e.cause }

// New returns an error whose message is format and args, as fmt.Sprintf
// formats them, and nothing more: the cause's own message never shows.
// It wraps cause: errors.Unwrap returns cause, and errors.Is and
// errors.AsType find cause and what it wraps, through Unwrap. A nil
// cause gives an error that wraps nothing, whose errors.Unwrap is nil.
func New(cause error, format string, args ...any) error {
	return &explained{msg: fmt.Sprintf(format, args...), cause: cause}
}

// Wrap returns an error whose message is format and args, as fmt.Sprintf
// formats them, then ": " and cause's message as it is, so that the
// message ends with cause's: as fmt.Errorf with a final ": %w" would
// make it, but with no check on the case of its first letter. It wraps
// cause as New does. A nil cause gives the message format and args give,
// alone, wrapping nothing.
func Wrap(cause error, format string, args ...any) error {
	words := fmt.Sprintf(format, args...)
	if cause == nil {
		return &explained{msg: words}
	}
	return &explained{msg: words + ": " + cause.Error(), cause: cause}
}
