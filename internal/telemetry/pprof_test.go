// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/telemetry"
)

// pprofPaths are the paths net/http/pprof serves that answer at once.
var pprofPaths = []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/symbol", "/debug/pprof/goroutine?debug=1"}

func TestPprofOffByDefault(t *testing.T) {
	t.Parallel()

	var c config.Telemetry
	mux := http.NewServeMux()
	telemetry.Pprof(mux, c.Pprof)
	for _, path := range pprofPaths {
		if got := serve(t, mux, http.MethodGet, path); got != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 with pprof off", path, got)
		}
	}
}

func TestPprofOnMountsOnTheGivenMux(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	telemetry.Pprof(mux, config.On)
	for _, path := range pprofPaths {
		if got := serve(t, mux, http.MethodGet, path); got != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200 with pprof on", path, got)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/pprof/", http.NoBody)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "goroutine") {
		t.Errorf("the pprof index lists no goroutine profile: %q", rec.Body.String())
	}

	// go tool pprof posts the addresses it wants named.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/debug/pprof/symbol", strings.NewReader("0x1"))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "num_symbols:") {
		t.Errorf("POST /debug/pprof/symbol: status %d, body %q; want 200 and the symbols", rec.Code, rec.Body.String())
	}
	if got := serve(t, mux, http.MethodPut, "/debug/pprof/symbol"); got != http.StatusMethodNotAllowed {
		t.Errorf("PUT /debug/pprof/symbol: status %d, want 405", got)
	}
}
