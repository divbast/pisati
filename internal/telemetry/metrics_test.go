// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package telemetry_test

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
)

// duration is the histogram's name in the Prometheus text format.
const duration = "http_server_request_duration_seconds"

func TestMetricsExposesRequestHistogram(t *testing.T) {
	t.Parallel()

	p := newProviders(t, config.Telemetry{})
	mux := http.NewServeMux()
	route(mux, "GET /p/{id}", ok)
	h := httpx.Chain(mux, p.HTTP(steppingClock(250*time.Millisecond)))

	if got := serve(t, h, http.MethodGet, "/p/123?q=quokka-query"); got != http.StatusOK {
		t.Fatalf("GET /p/123: status %d, want 200", got)
	}

	text := scrape(t, p)
	if !strings.Contains(text, "# TYPE "+duration+" histogram\n") {
		t.Errorf("no histogram %s in:\n%s", duration, text)
	}
	all := samples(t, text)
	series := map[string]string{
		"http_request_method":       "GET",
		"http_route":                "/p/{id}",
		"http_response_status_code": "200",
	}
	if got := value(t, all, duration+"_count", series); got != "1" {
		t.Errorf("count %s, want 1", got)
	}
	if got := value(t, all, duration+"_sum", series); got != "0.25" {
		t.Errorf("sum %s, want 0.25: the clock read once as the request arrived and once as it was answered", got)
	}
	// The buckets are the ones the semantic conventions give.
	for le, want := range map[string]string{"0.1": "0", "0.25": "1", "10": "1", "+Inf": "1"} {
		bucket := map[string]string{"le": le}
		maps.Copy(bucket, series)
		if got := value(t, all, duration+"_bucket", bucket); got != want {
			t.Errorf("bucket le=%s: %s, want %s", le, got, want)
		}
	}
	for _, s := range find(all, duration+"_count", nil) {
		for k := range s.labels {
			switch {
			case k == "http_request_method", k == "http_route", k == "http_response_status_code",
				strings.HasPrefix(k, "otel_scope_"):
			default:
				t.Errorf("the histogram has a label %s, want method, route and status code alone", k)
			}
		}
	}
	for _, leak := range []string{"/p/123", "quokka-query", "quokka-cookie", "203.0.113.7", "probe/1.0"} {
		if strings.Contains(text, leak) {
			t.Errorf("/metrics shows %q of the request:\n%s", leak, text)
		}
	}
}

func TestMetricsUsesRoutePattern(t *testing.T) {
	t.Parallel()

	p := newProviders(t, config.Telemetry{})
	mux := http.NewServeMux()
	route(mux, "GET /p/{id}", ok)
	route(mux, "/static/", ok)
	// A middleware between HTTP and the mux hands on a copy of the request,
	// as the session middleware does: the route still reaches HTTP.
	type key struct{}
	copying := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), key{}, "copy")))
		})
	}
	h := httpx.Chain(mux, p.HTTP(steppingClock(time.Millisecond)), copying)

	for _, req := range []struct {
		method, target string
		status         int
	}{
		{http.MethodGet, "/p/123", http.StatusOK},
		{http.MethodGet, "/p/456?q=1", http.StatusOK},
		{http.MethodGet, "/static/app.css", http.StatusOK},
		{http.MethodGet, "/quokka-nowhere", http.StatusNotFound},
		{http.MethodPost, "/p/789", http.StatusMethodNotAllowed},
		{"BREW", "/quokka-teapot", http.StatusNotFound},
	} {
		if got := serve(t, h, req.method, req.target); got != req.status {
			t.Fatalf("%s %s: status %d, want %d", req.method, req.target, got, req.status)
		}
	}

	text := scrape(t, p)
	all := samples(t, text)
	for _, want := range []struct {
		method, route, status, count string
	}{
		{"GET", "/p/{id}", "200", "2"},
		{"GET", "/static/", "200", "1"},
		{"GET", "unmatched", "404", "1"},
		{"POST", "unmatched", "405", "1"},
		{"_OTHER", "unmatched", "404", "1"},
	} {
		series := map[string]string{
			"http_request_method":       want.method,
			"http_route":                want.route,
			"http_response_status_code": want.status,
		}
		if got := value(t, all, duration+"_count", series); got != want.count {
			t.Errorf("%v: count %s, want %s", series, got, want.count)
		}
	}
	if n := len(find(all, duration+"_count", nil)); n != 5 {
		t.Errorf("%d series, want 5:\n%s", n, text)
	}
	for _, leak := range []string{"/p/123", "/p/456", "/p/789", "app.css", "quokka-nowhere", "quokka-teapot", "BREW"} {
		if strings.Contains(text, leak) {
			t.Errorf("/metrics shows %q of a request:\n%s", leak, text)
		}
	}
}

func TestHTTPRecordsTheStatusSent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
		status  string
	}{
		{
			name:    "nothing written",
			handler: func(http.ResponseWriter, *http.Request) {},
			status:  "200",
		},
		{
			name:    "a status written",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) },
			status:  "201",
		},
		{
			name: "flushed, then a late status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("Flush: %v", err)
				}
				w.WriteHeader(http.StatusInternalServerError)
			},
			status: "200",
		},
		{
			name:    "a panic before the header",
			handler: func(http.ResponseWriter, *http.Request) { panic("quokka") },
			status:  "500",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newProviders(t, config.Telemetry{})
			mux := http.NewServeMux()
			route(mux, "GET /x", tt.handler)
			h := httpx.Chain(mux, httpx.Recover(slog.New(slog.DiscardHandler)), p.HTTP(steppingClock(time.Millisecond)))
			serve(t, h, http.MethodGet, "/x")

			series := map[string]string{"http_route": "/x", "http_response_status_code": tt.status}
			if got := value(t, samples(t, scrape(t, p)), duration+"_count", series); got != "1" {
				t.Errorf("count %s, want 1", got)
			}
		})
	}
}
