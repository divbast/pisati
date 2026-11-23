// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// requestIDKey is the context key of a request's ID.
type requestIDKey struct{}

// RequestID gives every request an ID of its own: 128 bits from
// crypto/rand, as 32 lower-case hex digits. It never takes one from the
// request, whatever its headers say, so a client cannot choose the ID its
// log lines carry. It sets the ID as the response's X-Request-ID, and in
// the request's context, where RequestIDFrom finds it.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := random128()
			id := hex.EncodeToString(b[:])
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
		})
	}
}

// RequestIDFrom returns the ID RequestID gave the request ctx belongs to,
// and whether there is one: a context that did not pass through RequestID
// has none.
func RequestIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey{}).(string)
	return id, ok
}

// random128 returns 128 bits from crypto/rand.
func random128() [16]byte {
	var b [16]byte
	// Read never returns an error: it crashes the program instead.
	rand.Read(b[:])
	return b
}
