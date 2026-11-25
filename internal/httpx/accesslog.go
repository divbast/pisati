// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"log/slog"
	"net/http"
	"time"
)

// AccessLog logs each request to log once its handler returns, as one
// record at info level: the method, the path without its query, the
// status sent, the bytes of body written, how long the handler took by
// clock, in Go's text form for a duration, such as "1.5ms", and the
// request's ID (see RequestID), empty when it has none.
// It logs nothing else of the request: no query, no address and no
// header. A request whose handler panics is not logged here: Recover
// logs it.
func AccessLog(log *slog.Logger, clock func() time.Time) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := clock()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			id, _ := RequestIDFrom(r.Context())
			log.InfoContext(r.Context(), "request answered", "method", r.Method, "path", r.URL.Path,
				"status", rec.sentStatus(), "bytes", rec.bytes, "duration", clock().Sub(start).String(), "request_id", id)
		})
	}
}
