// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/divbast/pisati/internal/httpx"
)

func TestChainOrder(t *testing.T) {
	t.Parallel()

	var order []string
	mark := func(name string) httpx.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+" in")
				next.ServeHTTP(w, r)
				order = append(order, name+" out")
			})
		}
	}
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)

	httpx.Chain(h, mark("first"), mark("second"), mark("third")).ServeHTTP(httptest.NewRecorder(), req)
	want := []string{"first in", "second in", "third in", "handler", "third out", "second out", "first out"}
	if !slices.Equal(order, want) {
		t.Errorf("Chain ran %q, want %q", order, want)
	}

	order = nil
	httpx.Chain(h).ServeHTTP(httptest.NewRecorder(), req)
	if want := []string{"handler"}; !slices.Equal(order, want) {
		t.Errorf("Chain with no middleware ran %q, want %q", order, want)
	}
}
