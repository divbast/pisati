// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package telemetry

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/divbast/pisati/internal/httpx"
)

// unmatched is the route recorded for a request no route took, such as
// the mux's own 404s and 405s. Being one value, whatever paths clients
// ask for, it keeps the series bounded.
const unmatched = "unmatched"

// Metrics returns the handler for /metrics: every meter's readings, in
// Prometheus's text format, from the Providers' own registry. It serves
// at most 4 scrapes at once, and answers any more 503. The zero Providers
// is a bug, and Metrics panics on it.
func (p Providers) Metrics() http.Handler {
	p.made()
	return p.metrics
}

// HTTP returns middleware that records each request it passes on, once
// its handler returns: a server span, which is exported when traces are,
// and a reading, in seconds, of the http.server.request.duration
// histogram, in the semantic conventions' buckets. Both carry exactly
// three attributes:
//
//   - http.request.method: one of the methods the conventions know, GET,
//     HEAD, POST, PUT, DELETE, CONNECT, OPTIONS, TRACE, PATCH and QUERY,
//     or _OTHER for any other;
//   - http.route: what RecordRoute recorded, or "unmatched" when nothing
//     did;
//   - http.response.status_code: the status sent, which is 200 when the
//     handler wrote none. A handler that panics before writing one is
//     recorded as 500, which httpx.Recover then sends, with one exception:
//     a panic with http.ErrAbortHandler, which Recover lets abort the
//     response unanswered, is recorded as 500 all the same.
//
// Neither holds the request's path, query or headers, or the client's
// address, so a client can neither add a series nor put its data in a
// span.
//
// The span is named for the method and route, as in GET /p/{id}, or for
// the method alone when unmatched, and its status is an error for a 5xx or
// a panic. The handler's context carries it.
//
// The span continues the W3C trace that the request's traceparent and
// tracestate headers name only when the request's peer is a proxy trusted
// holds, by httpx.NewTrustedPeer's rule, and then follows that trace's
// sampling decision. Every other request, and one whose traceparent is
// malformed, starts a trace of its own, so a client cannot choose how its
// requests are traced. A nil or empty trusted trusts no peer. The
// propagator that reads the headers is the Providers' own, never OTel's
// global one.
//
// clock times the request: it is read once as the request arrives and
// once as its handler returns, and those two readings are the span's
// start and end. A nil clock, or the zero Providers, is a bug, and HTTP
// panics on it.
func (p Providers) HTTP(clock func() time.Time, trusted []netip.Prefix) httpx.Middleware {
	p.made()
	if clock == nil {
		panic("telemetry: HTTP needs a clock")
	}
	fromProxy := httpx.NewTrustedPeer(trusted)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := clock()
			method := requestMethod(r.Method)
			parent := r.Context()
			if fromProxy(r) {
				parent = p.propagator.Extract(parent, propagation.HeaderCarrier(r.Header))
			}
			ctx, span := p.tracer.Start(parent, spanName(method, ""),
				trace.WithSpanKind(trace.SpanKindServer), trace.WithTimestamp(start))
			m := &matched{}
			rec := &recorder{ResponseWriter: w}
			returned := false
			// Deferred, so that a request whose handler panics, which Recover
			// answers outside this middleware, is recorded too.
			defer func() {
				end := clock()
				status := rec.sent()
				if !returned && rec.status == 0 {
					status = http.StatusInternalServerError
				}
				route := m.route
				if route == "" {
					route = unmatched
				}
				attrs := attribute.NewSet(method, semconv.HTTPRoute(route), semconv.HTTPResponseStatusCode(status))
				span.SetName(spanName(method, m.route))
				span.SetAttributes(attrs.ToSlice()...)
				if !returned || status >= http.StatusInternalServerError {
					span.SetStatus(codes.Error, "")
				}
				span.End(trace.WithTimestamp(end))
				p.duration.RecordSet(ctx, end.Sub(start).Seconds(), attrs)
			}()
			next.ServeHTTP(rec, r.WithContext(context.WithValue(ctx, routeKey{}, m)))
			returned = true
		})
	}
}

// routeKey keys, in a request's context, where its route is recorded.
type routeKey struct{}

// matched is where RecordRoute records the route of a request HTTP is
// serving, for HTTP to read once the handler returns. A pointer to it
// rides in the context, so every copy of the request shares it.
type matched struct {
	// route is empty until RecordRoute records one.
	route string
}

// RecordRoute tells HTTP the route r was matched to: r.Pattern, which
// http.ServeMux sets on the request it hands a handler, without its method
// and host, so /p/{id} for the pattern GET /p/{id}. It is how a route
// reaches HTTP, which runs outside the mux and never sees the request the
// mux sets r.Pattern on: web.Route's handlers call it before anything
// else. The route comes from the pattern alone, never from the request's
// path.
//
// It is called on the goroutine serving r, before the handler returns. It
// does nothing for a request without a pattern, or one that did not come
// through HTTP. Called more than once, as nested muxes would, the last
// call wins.
func RecordRoute(r *http.Request) {
	m, ok := r.Context().Value(routeKey{}).(*matched)
	if !ok {
		return
	}
	// A pattern is [METHOD ][HOST]/PATH, and neither a method nor a host
	// holds a slash.
	if i := strings.IndexByte(r.Pattern, '/'); i >= 0 {
		m.route = r.Pattern[i:]
	}
}

// requestMethod returns method as the conventions record it: as itself
// when they know it, and as _OTHER otherwise, so that a client cannot add
// a series by inventing a method.
func requestMethod(method string) attribute.KeyValue {
	for _, known := range [...]attribute.KeyValue{
		semconv.HTTPRequestMethodGet, semconv.HTTPRequestMethodHead, semconv.HTTPRequestMethodPost,
		semconv.HTTPRequestMethodPut, semconv.HTTPRequestMethodDelete, semconv.HTTPRequestMethodConnect,
		semconv.HTTPRequestMethodOptions, semconv.HTTPRequestMethodTrace, semconv.HTTPRequestMethodPatch,
		semconv.HTTPRequestMethodQuery,
	} {
		if known.Value.AsString() == method {
			return known
		}
	}
	return semconv.HTTPRequestMethodOther
}

// spanName names a request's span as the conventions do: by its method
// and route, or by its method alone when it has no route, with HTTP
// standing for the method _OTHER.
func spanName(method attribute.KeyValue, route string) string {
	name := method.Value.AsString()
	if name == semconv.HTTPRequestMethodOther.Value.AsString() {
		name = "HTTP"
	}
	if route == "" {
		return name
	}
	return name + " " + route
}

// recorder keeps the status a handler sends. A flush through
// http.ResponseController reaches FlushError, which sends the header
// first, as net/http does; ResponseController finds every other feature
// through Unwrap.
type recorder struct {
	http.ResponseWriter

	// status is 0 until the header is sent.
	status int
}

func (r *recorder) WriteHeader(code int) {
	// A 1xx other than 101 leaves the final response still to come.
	final := code < http.StatusContinue || code >= http.StatusOK || code == http.StatusSwitchingProtocols
	if r.status == 0 && final {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// FlushError sends the header, if the handler has not, then flushes what
// is buffered to the client.
func (r *recorder) FlushError() error {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return http.NewResponseController(r.ResponseWriter).Flush()
}

// Unwrap returns the ResponseWriter r wraps, for http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// sent returns the status the response is sent with: 200 when the
// handler sent none, as net/http then sends.
func (r *recorder) sent() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
