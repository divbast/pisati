// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
)

// richCSP is the strict policy over https, widened as a rich post's page
// widens it.
func richCSP(nonce string) string {
	return "default-src 'none'; script-src 'self' 'nonce-" + nonce + "'; style-src 'self'; img-src 'self'; " +
		"font-src 'self'; connect-src 'self'; media-src https:; frame-src https:; form-action 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; object-src 'none'; style-src-attr 'unsafe-inline'; " +
		"report-to csp; upgrade-insecure-requests"
}

// rich widens the policy as a rich post's page does.
func rich(c *httpx.CSP) {
	c.AllowStyleAttributes()
	c.AllowHTTPSFrames()
	c.AllowHTTPSMedia()
}

func TestWithCSPWidensOneResponse(t *testing.T) {
	t.Parallel()

	// widened is what WithCSP returned to the last handler that called it.
	var widened bool
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter) {
		if _, err := io.WriteString(w, "<p>post</p>"); err != nil {
			t.Errorf("writing the body: %v", err)
		}
	}
	mux.HandleFunc("GET /rich", func(w http.ResponseWriter, r *http.Request) {
		// The handler's own header gives way to the policy.
		w.Header().Set("Content-Security-Policy", "default-src *")
		widened = httpx.WithCSP(r.Context(), rich)
		write(w)
	})
	mux.HandleFunc("GET /flushed", func(w http.ResponseWriter, r *http.Request) {
		widened = httpx.WithCSP(r.Context(), rich)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flushing: %v", err)
		}
	})
	mux.HandleFunc("GET /late", func(w http.ResponseWriter, r *http.Request) {
		write(w)
		widened = httpx.WithCSP(r.Context(), rich)
	})
	mux.HandleFunc("GET /plain", func(w http.ResponseWriter, _ *http.Request) {
		write(w)
	})
	mux.HandleFunc("GET /empty", func(_ http.ResponseWriter, r *http.Request) {
		// net/http writes the header once the handler returns.
		widened = httpx.WithCSP(r.Context(), rich)
	})
	h := httpx.Chain(mux, httpx.SecurityHeaders(baseURL(t, "https://blog.example"), config.HSTSHostOnly))

	tests := []struct {
		path  string
		widen bool
		want  func(nonce string) string
	}{
		{path: "/rich", widen: true, want: richCSP},
		{path: "/plain", want: func(n string) string { return strictCSP(n) + "; upgrade-insecure-requests" }},
		{path: "/flushed", widen: true, want: richCSP},
		{path: "/late", want: func(n string) string { return strictCSP(n) + "; upgrade-insecure-requests" }},
		{path: "/empty", widen: true, want: richCSP},
		{path: "/rich", widen: true, want: richCSP},
	}
	for _, tt := range tests {
		var nonce string
		widened = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, http.NoBody))
		// The nonce is the one the policy names, so read it back from there.
		got := rec.Result().Header.Values("Content-Security-Policy")
		if len(got) == 1 {
			nonce = nonceIn(got[0])
		}
		if want := tt.want(nonce); len(got) != 1 || got[0] != want {
			t.Errorf("GET %s: Content-Security-Policy = %q, want %q", tt.path, got, want)
		}
		if widened != tt.widen {
			t.Errorf("GET %s: WithCSP = %t, want %t", tt.path, widened, tt.widen)
		}
	}

	called := false
	if httpx.WithCSP(t.Context(), func(*httpx.CSP) { called = true }) || called {
		t.Errorf("WithCSP on a context SecurityHeaders never saw = true or ran its function, want false and not run")
	}
}

// nonceIn returns the nonce csp names, or "".
func nonceIn(csp string) string {
	_, rest, _ := strings.Cut(csp, "'nonce-")
	nonce, _, _ := strings.Cut(rest, "'")
	return nonce
}
