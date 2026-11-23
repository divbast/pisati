// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"encoding/base64"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
)

// strictCSP is the policy every response has unless its handler widens it,
// with nonce in place of <n>, over plain http.
func strictCSP(nonce string) string {
	return "default-src 'none'; script-src 'self' 'nonce-" + nonce + "'; style-src 'self'; img-src 'self'; " +
		"font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; " +
		"object-src 'none'; report-to csp"
}

// nonceHandler answers 204, keeping the nonce its request's context
// carries.
func nonceHandler(t *testing.T, nonce *string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, ok := httpx.Nonce(r.Context())
		if !ok {
			t.Error("Nonce found no nonce in the request's context")
		}
		*nonce = n
		w.WriteHeader(http.StatusNoContent)
	})
}

// get answers GET / with h, returning the response's header.
func get(t *testing.T, h http.Handler) http.Header {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
	return rec.Result().Header
}

func TestSecurityHeadersByScheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, base string
		hsts       config.HSTSScope
		wantHSTS   string // empty for none
	}{
		{name: "http localhost", base: "http://localhost:8080", hsts: config.HSTSHostOnly},
		{name: "http localhost, preload", base: "http://localhost:8080", hsts: config.HSTSPreload},
		{name: "http 127.0.0.1, subdomains", base: "http://127.0.0.1:8080", hsts: config.HSTSIncludeSubdomains},
		{name: "http [::1]", base: "http://[::1]:8080", hsts: config.HSTSHostOnly},
		{name: "https, host only", base: "https://blog.example", hsts: config.HSTSHostOnly, wantHSTS: "max-age=63072000"},
		{
			name: "https, subdomains", base: "https://blog.example", hsts: config.HSTSIncludeSubdomains,
			wantHSTS: "max-age=63072000; includeSubDomains",
		},
		{
			name: "https, preload", base: "https://blog.example:8443", hsts: config.HSTSPreload,
			wantHSTS: "max-age=63072000; includeSubDomains; preload",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var nonce string
			got := get(t, httpx.Chain(nonceHandler(t, &nonce), httpx.SecurityHeaders(baseURL(t, tt.base), tt.hsts, config.Dev{})))

			csp := strictCSP(nonce)
			if tt.wantHSTS != "" {
				csp += "; upgrade-insecure-requests"
			}
			if want := wantHeaders(tt.base, csp, tt.wantHSTS); !maps.EqualFunc(got, want, slices.Equal) {
				t.Errorf("headers:\n%v\nwant exactly:\n%v", got, want)
			}
		})
	}
}

// wantHeaders is every header SecurityHeaders sets for base, with csp as
// the policy and hsts as Strict-Transport-Security, or none when it is
// empty.
func wantHeaders(base, csp, hsts string) http.Header {
	want := http.Header{
		"Content-Security-Policy":      {csp},
		"Reporting-Endpoints":          {`csp="` + base + `/_/csp-report"`},
		"X-Content-Type-Options":       {"nosniff"},
		"Referrer-Policy":              {"strict-origin-when-cross-origin"},
		"Cross-Origin-Opener-Policy":   {"same-origin"},
		"Cross-Origin-Resource-Policy": {"same-origin"},
		"Permissions-Policy":           {"camera=(), microphone=(), geolocation=(), payment=(), usb=()"},
	}
	if hsts != "" {
		want.Set("Strict-Transport-Security", hsts)
	}
	return want
}

// devAt returns the dev settings with vite as the Vite dev server's URL,
// failing the test if it is not one.
func devAt(t *testing.T, vite string) config.Dev {
	t.Helper()
	u, err := config.ParseOptionalURL(vite)
	if err != nil {
		t.Fatalf("ParseOptionalURL(%q): %v", vite, err)
	}
	return config.Dev{ViteURL: u}
}

func TestSecurityHeadersDevMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, base, vite string
		// origin and live are the Vite server's origin and its HMR
		// socket's.
		origin, live string
		hsts         string
		upgrade      string
	}{
		{
			name: "localhost", base: "http://localhost:8080", vite: "http://localhost:5173",
			origin: "http://localhost:5173", live: "ws://localhost:5173",
		},
		{
			name: "127.0.0.1, with a path", base: "http://127.0.0.1:8080", vite: "http://127.0.0.1:5173/src/",
			origin: "http://127.0.0.1:5173", live: "ws://127.0.0.1:5173",
		},
		{
			name: "[::1]", base: "http://[::1]:8080", vite: "http://[::1]:5173",
			origin: "http://[::1]:5173", live: "ws://[::1]:5173",
		},
		{
			name: "https on localhost", base: "https://localhost:8443", vite: "https://localhost:5173",
			origin: "https://localhost:5173", live: "wss://localhost:5173",
			hsts: "max-age=63072000", upgrade: "; upgrade-insecure-requests",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var nonce string
			got := get(t, httpx.Chain(nonceHandler(t, &nonce),
				httpx.SecurityHeaders(baseURL(t, tt.base), config.HSTSHostOnly, devAt(t, tt.vite))))
			csp := "default-src 'none'; script-src 'self' 'nonce-" + nonce + "'; " +
				"style-src 'self' " + tt.origin + " 'nonce-" + nonce + "'; img-src 'self'; font-src 'self'; " +
				"connect-src 'self' " + tt.origin + " " + tt.live + "; form-action 'self'; frame-ancestors 'none'; " +
				"base-uri 'none'; object-src 'none'; report-to csp" + tt.upgrade
			if want := wantHeaders(tt.base, csp, tt.hsts); !maps.EqualFunc(got, want, slices.Equal) {
				t.Errorf("headers:\n%v\nwant exactly:\n%v", got, want)
			}
		})
	}
}

func TestSecurityHeadersRefusesUnsetArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base config.BaseURL
		hsts config.HSTSScope
		dev  config.Dev
	}{
		{name: "zero base URL", hsts: config.HSTSHostOnly},
		{name: "unknown HSTS scope", base: baseURL(t, "https://blog.example"), hsts: config.HSTSPreload + 1},
		{
			name: "a Vite server away from loopback", base: baseURL(t, "http://localhost:8080"),
			hsts: config.HSTSHostOnly, dev: devAt(t, "http://vite.example:5173"),
		},
		{
			name: "a Vite server for a base URL away from loopback", base: baseURL(t, "https://blog.example"),
			hsts: config.HSTSHostOnly, dev: devAt(t, "http://localhost:5173"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recover() == nil {
					t.Errorf("SecurityHeaders(%q, %v, %v) returned, want a panic: only a bug passes it", tt.base, tt.hsts, tt.dev)
				}
			}()
			httpx.SecurityHeaders(tt.base, tt.hsts, tt.dev)
		})
	}
}

func TestNonceUniquePerRequest(t *testing.T) {
	t.Parallel()

	var nonce string
	h := httpx.Chain(nonceHandler(t, &nonce), httpx.SecurityHeaders(baseURL(t, "https://blog.example"), config.HSTSHostOnly, config.Dev{}))
	seen := map[string]bool{}
	for range 200 {
		header := get(t, h)
		raw, err := base64.StdEncoding.DecodeString(nonce)
		if err != nil || len(raw) != 16 {
			t.Fatalf("the nonce %q is not 128 bits in base64: %d bytes, %v", nonce, len(raw), err)
		}
		if seen[nonce] {
			t.Fatalf("the nonce %q was given to two requests", nonce)
		}
		seen[nonce] = true
		if csp := header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self' 'nonce-"+nonce+"';") {
			t.Fatalf("Content-Security-Policy %q lacks the request's nonce %q", csp, nonce)
		}
	}

	if n, ok := httpx.Nonce(t.Context()); ok || n != "" {
		t.Errorf("Nonce(a context SecurityHeaders never saw) = %q, %t; want \"\", false", n, ok)
	}
}

func TestIsolationSetsCOEPOnlyWhenDeclared(t *testing.T) {
	t.Parallel()

	var zero httpx.Isolation
	tests := []struct {
		name string
		iso  httpx.Isolation
		want []string
	}{
		{name: "Isolated", iso: httpx.Isolated, want: []string{"require-corp"}},
		{name: "NotIsolated", iso: httpx.NotIsolated},
		{name: "zero", iso: zero},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var nonce string
			got := get(t, httpx.Chain(nonceHandler(t, &nonce),
				httpx.SecurityHeaders(baseURL(t, "https://blog.example"), config.HSTSHostOnly, config.Dev{}), tt.iso.Middleware()))
			if coep := got.Values("Cross-Origin-Embedder-Policy"); !slices.Equal(coep, tt.want) {
				t.Errorf("Cross-Origin-Embedder-Policy = %q, want %q", coep, tt.want)
			}
			if coop := got.Get("Cross-Origin-Opener-Policy"); coop != "same-origin" {
				t.Errorf("Cross-Origin-Opener-Policy = %q, want same-origin whatever the isolation", coop)
			}
		})
	}

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()

		defer func() {
			if recover() == nil {
				t.Error("Middleware on an unknown Isolation returned, want a panic: only a bug makes one")
			}
		}()
		(httpx.Isolated + 1).Middleware()
	})
}
