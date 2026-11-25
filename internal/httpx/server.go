// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/divbast/pisati/internal/config"
)

// Timeouts bound how long a server waits on a client, and how large a
// request's header may be. NewServer gives a field that is not positive
// its DefaultTimeouts value, so that no server it makes ever waits
// without a bound: the zero Timeouts is DefaultTimeouts.
type Timeouts struct {
	// ReadHeader bounds reading a request's header, from when the server
	// starts to read the request: as the connection opens, or, on a
	// connection kept alive, as the request's first bytes arrive.
	ReadHeader time.Duration
	// Read bounds reading a whole request, header and body, from the same
	// start.
	Read time.Duration
	// Write bounds writing a response, from the end of its request's
	// header.
	Write time.Duration
	// Idle bounds how long a connection kept alive waits for its next
	// request.
	Idle time.Duration
	// MaxHeaderBytes bounds a request's header, its request line
	// included. net/http reads up to 4 KiB past it before it answers 431.
	MaxHeaderBytes int
}

// DefaultTimeouts returns the Timeouts Pisati serves with: 10 s to read a
// request's header and 30 s the whole request, 60 s to write a response,
// 120 s idle, and a header of up to 64 KiB.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		ReadHeader:     10 * time.Second,
		Read:           30 * time.Second,
		Write:          60 * time.Second,
		Idle:           120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
}

// NewServer returns a server that serves h on addr within t's bounds,
// each field of t that is not positive taking its DefaultTimeouts value.
// The server's own errors, which net/http writes to its ErrorLog, go to
// log at error level. Starting and stopping it are the caller's.
func NewServer(addr config.ListenAddr, h http.Handler, t Timeouts, log *slog.Logger) *http.Server {
	d := DefaultTimeouts()
	return &http.Server{
		Addr:              addr.String(),
		Handler:           h,
		ReadHeaderTimeout: positive(t.ReadHeader, d.ReadHeader),
		ReadTimeout:       positive(t.Read, d.Read),
		WriteTimeout:      positive(t.Write, d.Write),
		IdleTimeout:       positive(t.Idle, d.Idle),
		MaxHeaderBytes:    positive(t.MaxHeaderBytes, d.MaxHeaderBytes),
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
}

// positive returns v when it is above zero, and otherwise def. A zero or
// negative bound would leave net/http waiting without one.
func positive[T time.Duration | int](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}
