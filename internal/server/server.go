// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package server serves the page on the app port, and
// /healthz on the health port, each an allowlist that answers everything
// it does not serve with a 404 and an empty body.
package server

import (
	"io"
	"log/slog"
	"net/http"
)

// Page is exactly what GET / answers.
const Page = "Hello from pisati"

// App serves the page: GET or HEAD on / answers 200 with Page, and
// anything else 404 with nothing.
func App() http.Handler {
	return only("/", Page)
}

// only answers GET or HEAD on path alone, with body, and anything else 404
// with an empty body.
func only(path, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		io.WriteString(w, body) //nolint:errcheck // the status is sent, and a failed write means the client has gone: no one is left to answer
	})
}

// Logged logs each request h answers to log, once answered: its method,
// path and status.
func Logged(h http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		log.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path, "status", rec.status)
	})
}

// statusRecorder keeps the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
