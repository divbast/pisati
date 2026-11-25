// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
)

func TestCrossOriginRefusesForeignPost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, method, host string
		header             map[string]string
		want               int
	}{
		{
			name: "cross-site", method: http.MethodPost, host: "blog.example",
			header: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://foreign.example"},
			want:   http.StatusForbidden,
		},
		{
			name: "same-site", method: http.MethodPost, host: "blog.example",
			header: map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://other.blog.example"},
			want:   http.StatusForbidden,
		},
		{
			name: "foreign origin, no Sec-Fetch-Site", method: http.MethodPut, host: "blog.example",
			header: map[string]string{"Origin": "https://foreign.example"},
			want:   http.StatusForbidden,
		},
		{
			name: "same-origin", method: http.MethodPost, host: "blog.example",
			header: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://blog.example"},
			want:   http.StatusOK,
		},
		{
			// Behind a reverse proxy that passes another Host, the base
			// URL's origin is still trusted.
			name: "the base URL's origin, another Host", method: http.MethodPost, host: "127.0.0.1:8080",
			header: map[string]string{"Origin": "https://blog.example"},
			want:   http.StatusOK,
		},
		{
			name: "no browser headers", method: http.MethodPost, host: "blog.example",
			want: http.StatusOK,
		},
		{
			name: "cross-site GET", method: http.MethodGet, host: "blog.example",
			header: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://foreign.example"},
			want:   http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			served := false
			h := httpx.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }),
				httpx.CrossOrigin(baseURL(t, "https://blog.example")))
			req := httptest.NewRequestWithContext(t.Context(), tt.method, "/_/theme", http.NoBody)
			req.Host = tt.host
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("%s with %v = %d, want %d", tt.method, tt.header, rec.Code, tt.want)
			}
			if served != (tt.want == http.StatusOK) {
				t.Errorf("%s with %v reached the handler: %t, want %t", tt.method, tt.header, served, tt.want == http.StatusOK)
			}
			if tt.want == http.StatusForbidden && rec.Body.String() != "Forbidden\n" {
				t.Errorf("refused with the body %q, want the generic \"Forbidden\\n\"", rec.Body.String())
			}
		})
	}

	t.Run("zero base URL", func(t *testing.T) {
		t.Parallel()

		defer func() {
			if recover() == nil {
				t.Error("CrossOrigin(the zero BaseURL) returned, want a panic: only a bug passes it")
			}
		}()
		var zero config.BaseURL
		httpx.CrossOrigin(zero)
	})
}

func TestCrossOriginRefusalIsNotStored(t *testing.T) {
	t.Parallel()

	h := httpx.CrossOrigin(baseURL(t, "https://blog.example"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a refused request reached the handler")
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/_/theme", http.NoBody)
	req.Host = "blog.example"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://foreign.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a cross-site POST = %d, want 403", rec.Code)
	}
	// The refusal answers this request alone: no cache may keep it.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("the 403 has Cache-Control %q, want no-store", cc)
	}
}
