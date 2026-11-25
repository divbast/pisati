// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package auth holds Pisati's rules for passwords, password hashes and
// sessions, as pure functions that take values and return decisions. It
// reads no clock, draws no random numbers and does no I/O: callers pass
// the time and the salt.
//
// A password enters as a Password, from ParsePassword, and a stored hash
// as a Hash, from ParseHash. CheckPassword decides whether a new password
// may be used, HashPassword hashes it with argon2id, and Verify says
// whether a password matches a hash. Evaluate says whether a session is
// still live, and StepUpFresh whether a sign-in is recent enough for a
// sensitive action.
package auth

import (
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// The bounds of a password (no other rule applies to what it holds).
const (
	// MinPasswordRunes is the fewest characters a password has, counted as
	// Unicode code points.
	MinPasswordRunes = 15
	// MaxPasswordBytes is the most bytes a password has, in UTF-8.
	MaxPasswordBytes = 1024
)

// The ways ParsePassword refuses a password. Each is returned as it is,
// unwrapped, so its message is fixed and never holds the password.
var (
	// ErrPasswordTooShort is a password of fewer than MinPasswordRunes
	// characters.
	ErrPasswordTooShort = errors.New("the password has fewer than 15 characters")
	// ErrPasswordTooLong is a password of more than MaxPasswordBytes bytes.
	ErrPasswordTooLong = errors.New("the password is longer than 1,024 bytes")
	// ErrPasswordNotUTF8 is a password that is not valid UTF-8.
	ErrPasswordNotUTF8 = errors.New("the password is not valid UTF-8 text")
)

// ErrInvalid is a value that one of this package's rules refuses, other
// than a password: a salt, Params or a stored hash. Every error the package returns for one wraps it and says which
// rule, without repeating the value, in plain words of its own: ErrInvalid's
// text does not show in it.
var ErrInvalid = errors.New("auth: invalid")

// refusal is an error the package returns for a value its rules refuse:
// its message is msg alone, and it wraps ErrInvalid. The core imports no
// package outside the standard library and itself, so it has this type of
// its own.
type refusal struct {
	msg string
}

func (r *refusal) Error() string { return r.msg }
func (r *refusal) Unwrap() error { return ErrInvalid }

// refuse returns the refusal format and args say.
func refuse(format string, args ...any) error {
	return &refusal{msg: fmt.Sprintf(format, args...)}
}

// redacted stands in for a secret wherever one would show: all of a
// Password, and a Hash's salt and key.
const redacted = "[redacted]"

// Password is a password exactly as it was given: never trimmed, folded or
// normalised, so a password is hashed and checked as the user typed it. It
// holds valid UTF-8 of MinPasswordRunes characters to MaxPasswordBytes
// bytes. fmt, slog and encoding/json show it as [redacted], also when it
// sits in another value's unexported field.
//
// The zero Password is the empty password, which ParsePassword refuses:
// HashPassword and CheckPassword return ErrPasswordTooShort for it, and
// Verify returns Mismatch. Passwords do not compare with ==, which would
// take time that depends on how much of them matched.
type Password struct {
	// A function rather than the string: fmt prints a function as its
	// address, so a Password in an unexported field of another value,
	// where fmt cannot call Format, still does not print the password.
	reveal func() string
}

// ParsePassword returns s as a Password, or, checked in this order,
// ErrPasswordTooLong when s has more than MaxPasswordBytes bytes,
// ErrPasswordNotUTF8 when it is not valid UTF-8, and ErrPasswordTooShort
// when it has fewer than MinPasswordRunes characters. It never changes s.
func ParsePassword(s string) (Password, error) {
	// The length first, so that an oversized input costs nothing to refuse.
	if len(s) > MaxPasswordBytes {
		return Password{}, ErrPasswordTooLong
	}
	if !utf8.ValidString(s) {
		return Password{}, ErrPasswordNotUTF8
	}
	if utf8.RuneCountInString(s) < MinPasswordRunes {
		return Password{}, ErrPasswordTooShort
	}
	return Password{reveal: func() string { return s }}, nil
}

// value returns the password, or "" for the zero Password.
func (p Password) value() string {
	if p.reveal == nil {
		return ""
	}
	return p.reveal()
}

// Format prints the password as [redacted], whatever the verb and flags.
func (Password) Format(f fmt.State, _ rune) {
	io.WriteString(f, redacted) //nolint:errcheck // fmt.State writes into fmt's own buffer, which cannot fail, and Format has no error to return
}

// MarshalText returns [redacted], so encoding/json and slog's handlers,
// which use it, never show the password. The core does not import
// log/slog, so a Password has no LogValue method.
func (Password) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}
