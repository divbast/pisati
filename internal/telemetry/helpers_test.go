// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package telemetry_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/telemetry"
)

// testVersion is the version every test's Providers report.
const testVersion = "1.2.3-test"

// clientTrace is the trace every test request names, in a traceparent
// header of its own, as a client may.
const clientTrace = "4bf92f3577b34da6a3ce929d0e0e4736"

// start is when every test's clock starts.
var start = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)

// steppingClock returns a clock that reads start, then a step later at
// each call after.
func steppingClock(step time.Duration) func() time.Time {
	var mu sync.Mutex
	now := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := now
		now = now.Add(step)
		return t
	}
}

// newProviders returns Providers made from c, shut down when the test
// ends if the test has not shut them down itself.
func newProviders(t *testing.T, c config.Telemetry) telemetry.Providers {
	t.Helper()
	p, err := telemetry.NewProviders(t.Context(), c, testVersion, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewProviders: %v", err)
	}
	t.Cleanup(func() { shutdown(t, p) })
	return p
}

// shutdown shuts p down, failing the test if that fails. The test's
// context ends before its cleanups run, so this has a context of its own.
func shutdown(t *testing.T, p telemetry.Providers) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// route registers h on mux at pattern as web.Route does: the handler it
// registers reports the matched route before anything else.
func route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		telemetry.RecordRoute(r)
		h(w, r)
	})
}

// ok answers 200 with a short body.
func ok(w http.ResponseWriter, _ *http.Request) {
	io.WriteString(w, "ok") //nolint:errcheck // a test recorder's write cannot fail
}

// serve sends h a request, and returns the status it answers with.
func serve(t *testing.T, h http.Handler, method, target string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, http.NoBody)
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("Cookie", "pisati-session=quokka-cookie")
	req.Header.Set("User-Agent", "probe/1.0")
	req.Header.Set("Traceparent", "00-"+clientTrace+"-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// scrape returns what p's metrics handler serves, failing the test
// unless it answers 200.
func scrape(t *testing.T, p telemetry.Providers) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody)
	rec := httptest.NewRecorder()
	p.Metrics().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status %d, body %q", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// sample is one line of the Prometheus text format: a series and its value.
type sample struct {
	name   string
	labels map[string]string
	value  string
}

// labelRE matches one label of a series, and the comma after it.
var labelRE = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)",?`)

// samples returns every sample in text, the Prometheus text format,
// failing the test on a line it cannot read.
func samples(t *testing.T, text string) []sample {
	t.Helper()
	var out []sample
	for line := range strings.Lines(text) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s := sample{labels: map[string]string{}}
		rest := line
		if i := strings.IndexAny(rest, "{ "); i >= 0 && rest[i] == '{' {
			s.name, rest = rest[:i], rest[i+1:]
			for !strings.HasPrefix(rest, "}") {
				m := labelRE.FindStringSubmatch(rest)
				if m == nil {
					t.Fatalf("cannot read the labels of %q", line)
				}
				s.labels[m[1]] = m[2]
				rest = rest[len(m[0]):]
			}
			rest = strings.TrimPrefix(rest, "}")
		} else if i >= 0 {
			s.name, rest = rest[:i], rest[i:]
		}
		s.value = strings.TrimSpace(rest)
		out = append(out, s)
	}
	return out
}

// find returns the samples named name whose labels include want.
func find(all []sample, name string, want map[string]string) []sample {
	var out []sample
	for _, s := range all {
		if s.name != name {
			continue
		}
		match := true
		for k, v := range want {
			if s.labels[k] != v {
				match = false
			}
		}
		if match {
			out = append(out, s)
		}
	}
	return out
}

// value returns the value of the one sample named name whose labels
// include want, failing the test unless there is exactly one.
func value(t *testing.T, all []sample, name string, want map[string]string) string {
	t.Helper()
	got := find(all, name, want)
	if len(got) != 1 {
		t.Fatalf("%s%v: %d samples, want 1; all of them: %v", name, want, len(got), all)
	}
	return got[0].value
}

// collector is a stand-in OTLP/HTTP collector on loopback. It keeps every
// request it is sent, and answers each 200 with an empty body.
type collector struct {
	url string

	mu   sync.Mutex
	reqs []received
}

// received is one request a collector was sent.
type received struct {
	path   string
	header http.Header
	body   []byte
}

// newCollector starts a collector, closed when the test ends.
func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("collector: reading the body: %v", err)
		}
		c.mu.Lock()
		c.reqs = append(c.reqs, received{path: r.URL.Path, header: r.Header.Clone(), body: body})
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL
	return c
}

// requests returns every request the collector has been sent so far.
func (c *collector) requests() []received {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reqs)
}

// endpoint returns s as a config.OptionalURL, failing the test if it is
// not one.
func endpoint(t *testing.T, s string) config.OptionalURL {
	t.Helper()
	u, err := config.ParseOptionalURL(s)
	if err != nil {
		t.Fatalf("ParseOptionalURL(%q): %v", s, err)
	}
	return u
}
