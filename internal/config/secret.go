// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
)

// SecretSource says where a secret comes from: FromFile, FromCredential
// or Generated. Load records the source and never reads the secret.
//
//sumtype:decl
type SecretSource interface {
	secretSource()
}

// FromFile reads the secret from the file at Path. In a Config from Load,
// Path is absolute and clean.
type FromFile struct {
	Path string
}

// FromCredential reads the secret from the systemd credential Name, in
// $CREDENTIALS_DIRECTORY.
type FromCredential struct {
	Name string
}

// Generated means no secret was given, so the consumer decides what to
// do without one.
type Generated struct{}

func (FromFile) secretSource()       {}
func (FromCredential) secretSource() {}
func (Generated) secretSource()      {}

// redacted is all a Secret ever shows of itself.
const redacted = "[redacted]"

// Secret holds a secret's bytes and keeps them out of logs and printed
// output: slog and every fmt verb show [redacted]. The zero Secret holds
// nothing.
type Secret struct {
	// A function returning a copy of the bytes, rather than the bytes or
	// a pointer to them: fmt prints a function as its address alone, so
	// a Secret in an unexported field of another value, where fmt cannot
	// call Format, still does not print them.
	reveal func() []byte
}

// NewSecret returns a Secret holding a copy of b, or an error wrapping
// ErrInvalid when b is empty.
func NewSecret(b []byte) (Secret, error) {
	if len(b) == 0 {
		return Secret{}, refuse(ErrInvalid, "config.NewSecret was given no bytes, but a secret is never empty. This "+
			"is a bug in Pisati")
	}
	held := slices.Clone(b)
	return Secret{reveal: func() []byte { return slices.Clone(held) }}, nil
}

// Reveal returns a copy of the secret's bytes, which the caller may
// change; nil for the zero Secret.
func (s Secret) Reveal() []byte {
	if s.reveal == nil {
		return nil
	}
	return s.reveal()
}

// LogValue shows the secret to slog as [redacted].
func (Secret) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

// Format prints the secret as [redacted], whatever the verb and flags.
func (Secret) Format(f fmt.State, _ rune) {
	io.WriteString(f, redacted) //nolint:errcheck // fmt.State writes into fmt's own buffer, which cannot fail, and Format has no error to return
}
