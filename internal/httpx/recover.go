// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"runtime/debug"
)

// Recover answers a request whose handler panics with 500 and a generic
// body, and logs the panic to log at error level, with the request's ID
// (see RequestID), its method and path, and the stack. It logs what the
// handler panicked with only when that is a runtime error, as any other
// value might hold the request's data. The connection stays open for the
// client's next request.
//
// The 500 has Cache-Control: no-store, so no cache keeps it, and none of
// the cookies, validators or representation headers the handler had set
// for the response it meant to send. Every other header stays as it is,
// so the ones SecurityHeaders set still go out.
//
// A handler that panics once its response has begun can no longer be
// given a status: Recover logs the panic, then aborts the response, so
// the client sees it cut off rather than take what it has for the whole.
// A handler that panics with http.ErrAbortHandler has chosen to abort:
// Recover lets it, and logs nothing, as net/http does.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			rec := &recorder{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// net/http aborts the response without a log line for
				// http.ErrAbortHandler alone.
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(http.ErrAbortHandler)
				}
				id, _ := RequestIDFrom(ctx)
				log.ErrorContext(ctx, "request handler panicked", "request_id", id, "method", r.Method,
					"path", r.URL.Path, "panic", describePanic(v), "stack", string(debug.Stack()))
				if rec.wroteHeader {
					panic(http.ErrAbortHandler)
				}
				forError(w.Header())
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// forError readies h, which holds what a handler set for the response it
// never sent, for the 500 sent instead: it can be stored by no cache, and
// carries none of the handler's cookies, validators or representation
// metadata. http.Error drops only Content-Length, and resets the content
// type.
func forError(h http.Header) {
	for _, name := range [...]string{
		"Set-Cookie", "ETag", "Last-Modified", "Expires", "Content-Disposition",
		"Content-Encoding", "Content-Language", "Content-Location", "Content-Range",
	} {
		h.Del(name)
	}
	h.Set("Cache-Control", "no-store")
}

// describePanic describes the value a handler panicked with, for the log.
// A runtime error's message holds no data of the request's. Any other
// value might, so only its type is given.
func describePanic(v any) string {
	if re, ok := v.(runtime.Error); ok {
		return re.Error()
	}
	return fmt.Sprintf("a value of type %T", v)
}
