// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package telemetry

import (
	"io"
	"log/slog"
	"strconv"

	"github.com/divbast/pisati/internal/config"
)

// NewLogger returns a logger that writes each record at c.Level or above
// to w, as one line: a JSON object for config.LogJSON, or slog's
// key=value text for config.LogText. It is not made the default logger:
// it reaches code only as a parameter.
//
// It changes no value it is given. A secret is kept out of the log by its
// own type, not by the logger: config.Secret and the other secret types
// show slog [redacted], through LogValue or MarshalText, in both formats
// and inside groups.
//
// A nil w, or a format outside config's, is a bug, and NewLogger panics
// on it.
func NewLogger(w io.Writer, c config.Log) *slog.Logger {
	if w == nil {
		panic("telemetry.NewLogger was given no writer, " + bug)
	}
	opts := &slog.HandlerOptions{Level: c.Level}
	switch c.Format {
	case config.LogJSON:
		return slog.New(slog.NewJSONHandler(w, opts))
	case config.LogText:
		return slog.New(slog.NewTextHandler(w, opts))
	}
	panic("telemetry.NewLogger was given the log format " + strconv.Itoa(int(c.Format)) + ", which it does not know, " + bug)
}
