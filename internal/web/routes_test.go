// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
	"github.com/divbast/pisati/internal/telemetry"
	"github.com/divbast/pisati/internal/web"
)

// route is one route the site serves, as it is declared: a request that
// reaches it, and what Route applies to it.
type route struct {
	method, target string
	// pattern is the route's pattern without its method, as telemetry
	// records it.
	pattern  string
	isolated bool
	limit    httpx.ByteLimit
	// contentType is what a request to it sends its body as.
	contentType string
}

// siteRoutes is every route the site serves, as NewSite declares them.
var siteRoutes = []route{
	{method: http.MethodGet, target: "/", pattern: "/{$}"},
	{method: http.MethodGet, target: "/login", pattern: "/login", isolated: true},
	{method: http.MethodPost, target: "/login", pattern: "/login", isolated: true, limit: httpx.FormLimit},
	{method: http.MethodPost, target: "/_/theme", pattern: "/_/theme", limit: httpx.FormLimit},
	{
		method: http.MethodPost, target: "/_/csp-report", pattern: "/_/csp-report", limit: httpx.CSPReportLimit,
		contentType: "application/reports+json",
	},
	{method: http.MethodGet, target: "/static/" + script, pattern: "/static/"},
	{method: http.MethodGet, target: "/nothing-here", pattern: "/"},
}

func TestEveryRouteDeclaresAccess(t *testing.T) {
	t.Parallel()

	providers, err := telemetry.NewProviders(t.Context(), config.Telemetry{}, "test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if err := providers.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	p := newPisati(t, options{})
	p.h = providers.HTTP(time.Now, nil)(p.h)

	for _, rt := range siteRoutes {
		header := map[string]string{}
		if rt.contentType != "" {
			header["Content-Type"] = rt.contentType
		}
		// Within its limit, a route answers with its own isolation.
		w := p.do(request{method: rt.method, target: rt.target, header: header})
		if w.Code == http.StatusRequestEntityTooLarge {
			t.Errorf("%s %s with no body = 413", rt.method, rt.target)
		}
		coep := w.Header().Get("Cross-Origin-Embedder-Policy")
		if rt.isolated != (coep == "require-corp") {
			t.Errorf("%s %s: Cross-Origin-Embedder-Policy %q, want isolated %t", rt.method, rt.target, coep, rt.isolated)
		}
		// Past it, by a byte, the body is refused.
		over := strings.Repeat("a", int(rt.limit)+1)
		w = p.do(request{method: rt.method, target: rt.target, body: over, length: int64(len(over)), header: header})
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s %s with a body of %d bytes = %d, want 413", rt.method, rt.target, len(over), w.Code)
		}
	}

	// Each route recorded its pattern, which only Route's wrapper does.
	scrape := httptest.NewRecorder()
	providers.Metrics().ServeHTTP(scrape, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	metrics, err := io.ReadAll(scrape.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range siteRoutes {
		if !strings.Contains(string(metrics), `http_route="`+rt.pattern+`"`) {
			t.Errorf("no request was recorded for the route %s: Route did not serve %s %s", rt.pattern, rt.method, rt.target)
		}
	}
	if strings.Contains(string(metrics), `http_route="unmatched"`) {
		t.Error("a request reached a handler Route did not wrap")
	}
}

func TestSignedInRouteNeedsASession(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	st := newSite(t, s, origin, time.Now)
	mux := http.NewServeMux()
	reached := false
	web.Route(mux, "GET /drafts", &web.SignedIn{}, httpx.NotIsolated, 0, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	h := web.SessionMiddleware(st.sessions, st.log)(mux)

	w := get(t, h, http.MethodGet, "/drafts?page=2")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2Fdrafts%3Fpage%3D2" || reached {
		t.Errorf("signed out: %d to %q, reached %t; want 303 to /login?next=%%2Fdrafts%%3Fpage%%3D2, not reached",
			w.Code, w.Header().Get("Location"), reached)
	}

	c := st.renew(t, 7)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/drafts", nil)
	r.AddCookie(requestCookie(c.Name, c.Value))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !reached {
		t.Errorf("signed in: %d, reached %t; want the handler's 200", w.Code, reached)
	}
}

func TestRoutePanicsOnABug(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for name, register := range map[string]func(){
		"no access":  func() { web.Route(http.NewServeMux(), "GET /x", nil, httpx.NotIsolated, 0, ok) },
		"no mux":     func() { web.Route(nil, "GET /x", &web.Public{}, httpx.NotIsolated, 0, ok) },
		"no handler": func() { web.Route(http.NewServeMux(), "GET /x", &web.Public{}, httpx.NotIsolated, 0, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recover() == nil {
					t.Error("Route returned, want a panic: only a bug registers a route so")
				}
			}()
			register()
		})
	}
}

func TestNewSiteRefuses(t *testing.T) {
	t.Parallel()

	site, err := web.NewSite(nil, config.BaseURL{}, nil, nil, nil)
	if err == nil || site != nil {
		t.Fatalf("NewSite(nothing) = %v, %v; want nil and an error", site, err)
	}
	for _, want := range []string{"assets", "base URL", "limiter", "ClientIP", "logger"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name the %s", err, want)
		}
	}
}
