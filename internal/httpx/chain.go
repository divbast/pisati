// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package httpx serves Pisati over HTTP, and sends its requests out. It
// has a server that bounds every wait on a client, and the middleware a
// request passes through on its way to a handler. That gives each
// request an ID, recovers a handler's panic, logs the request without
// its query or the client's address, sets the security headers and a
// strict CSP with a nonce of its own, refuses cross-origin requests that
// change state, holds each route's body to a limit, and limits how often
// each client is served, finding the client behind trusted proxies.
// Every request Pisati sends goes through a GuardedClient, an
// address-checked client that refuses private and reserved addresses
// and bounds each exchange.
package httpx

import (
	"net/http"
	"slices"
)

// Middleware wraps a handler in behaviour of its own, and returns the
// handler that has it.
type Middleware func(http.Handler) http.Handler

// Chain returns h wrapped in m, the first outermost: a request passes
// through m[0], then m[1] and the rest in turn, then reaches h, and the
// response passes back out the other way. With no m, Chain returns h. A
// nil Middleware is a bug, and panics.
func Chain(h http.Handler, m ...Middleware) http.Handler {
	for _, wrap := range slices.Backward(m) {
		h = wrap(h)
	}
	return h
}

// recorder wraps a handler's ResponseWriter to see what the handler
// sends: whether the header has been written, with what status, and how
// many bytes of body. beforeHeader, when it is not nil, runs once, just
// before the header is written, and may still change it.
//
// A flush through http.ResponseController reaches FlushError, which
// writes the header first, as net/http does; ResponseController finds
// every other feature through Unwrap.
type recorder struct {
	http.ResponseWriter

	beforeHeader func(http.Header)
	wroteHeader  bool
	status       int
	bytes        int64
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader && !informational(code) {
		r.wroteHeader = true
		r.status = code
		if r.beforeHeader != nil {
			r.beforeHeader(r.Header())
		}
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// FlushError writes the header, if the handler has not, then flushes
// what is buffered to the client.
func (r *recorder) FlushError() error {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(r.ResponseWriter).Flush()
}

// Unwrap returns the ResponseWriter r wraps, for http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// sentStatus returns the status the response is sent with: 200 when the
// handler wrote no header, as net/http then sends.
func (r *recorder) sentStatus() int {
	if !r.wroteHeader {
		return http.StatusOK
	}
	return r.status
}

// informational reports whether code is a 1xx status that leaves the
// final response still to come: every one but 101 Switching Protocols.
func informational(code int) bool {
	return code >= 100 && code < 200 && code != http.StatusSwitchingProtocols
}
