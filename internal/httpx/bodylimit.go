// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"errors"
	"net/http"
)

// ByteLimit is how many bytes of body a request or a response may have.
// BodyLimit's zero ByteLimit allows none, as does a negative one.
type ByteLimit int64

// The limits Pisati's routes hold bodies to, by what a route accepts.
const (
	// FormLimit is for a form a browser posts: 64 KiB.
	FormLimit ByteLimit = 64 << 10
	// JSONLimit is for a JSON document: 1 MiB.
	JSONLimit ByteLimit = 1 << 20
	// CSPReportLimit is for a browser's report of a CSP violation: 16 KiB.
	CSPReportLimit ByteLimit = 16 << 10
)

// BodyLimit holds a request's body to n bytes. A request that declares a
// Content-Length over n is answered 413, with a generic body, before the
// handler runs. Any other body fails to read past n bytes with an
// *http.MaxBytesError, which the handler answers with 413 by passing it
// to RefuseTooLarge.
func BodyLimit(n ByteLimit) Middleware {
	limit := max(int64(n), 0)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				refuseTooLarge(w)
				return
			}
			// A copy, as a handler must not change the request it is given.
			limited := *r
			limited.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, &limited)
		})
	}
}

// RefuseTooLarge answers 413, with the generic body BodyLimit answers
// with, when err is or wraps the *http.MaxBytesError of reading a body
// past its limit, and reports whether it did. A handler passes it any
// error from reading the body before it handles the error another way.
func RefuseTooLarge(w http.ResponseWriter, err error) bool {
	if _, ok := errors.AsType[*http.MaxBytesError](err); !ok {
		return false
	}
	refuseTooLarge(w)
	return true
}

// refuseTooLarge answers 413 with a generic body.
func refuseTooLarge(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
}
