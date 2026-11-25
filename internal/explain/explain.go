// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package explain words what Pisati tells the operator. It makes errors
// whose message is a sentence of Pisati's own around the error that
// caused it: the message says what happened in plain words, and
// errors.Is, errors.As and errors.AsType still find the cause, and
// everything the cause wraps, through it. Bytes writes a size the one way
// every message writes one.
//
// Its errors exist so that a message can start with a capital letter, as
// "Pisati cannot …" does, and so that a sentinel's own word, such as
// "invalid", never shows: fmt.Errorf with %w would print the cause's
// message wherever the verb stands.
package explain

import (
	"fmt"
	"strconv"
)

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

// Bytes returns n as a count of bytes, its digits in groups of three
// joined by commas, as in "1,048,576 bytes", or "1 byte" for 1. A message
// that gives a size or a limit writes it so, from the number itself, so
// that no unit written beside it can disagree with it.
func Bytes(n int64) string {
	return Count(n, "byte", "bytes")
}

// Count returns n, its digits in groups of three joined by commas, then
// one, the singular, when n is 1, and many otherwise: "1 value", "0
// values", "1,500 values". A message that prints a count writes it so.
func Count(n int64, one, many string) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if n < 0 {
		sign, digits = "-", digits[1:]
	}
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	if n == 1 {
		return "1 " + one
	}
	return sign + digits + " " + many
}
