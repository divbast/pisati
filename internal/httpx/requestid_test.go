// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/divbast/pisati/internal/httpx"
)

func TestRequestID(t *testing.T) {
	t.Parallel()

	var seen string
	var found bool
	h := httpx.Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, found = httpx.RequestIDFrom(r.Context())
	}), httpx.RequestID())

	hex128 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	ids := map[string]bool{}
	for range 100 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("X-Request-ID", "chosen-by-the-client")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		got := rec.Result().Header.Get("X-Request-ID")
		if !found || seen != got {
			t.Fatalf("RequestIDFrom = %q, %t; want the response's X-Request-ID %q, true", seen, found, got)
		}
		if !hex128.MatchString(got) {
			t.Fatalf("X-Request-ID = %q, want 128 bits as 32 lower-case hex digits", got)
		}
		if ids[got] {
			t.Fatalf("X-Request-ID %q given to two requests", got)
		}
		ids[got] = true
	}

	if id, ok := httpx.RequestIDFrom(t.Context()); ok || id != "" {
		t.Errorf("RequestIDFrom(a context RequestID never saw) = %q, %t; want \"\", false", id, ok)
	}
}
