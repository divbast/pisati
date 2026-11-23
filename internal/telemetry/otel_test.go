// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package telemetry_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"go.uber.org/goleak"
	"google.golang.org/protobuf/proto"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
	"github.com/divbast/pisati/internal/telemetry"
)

func TestNoOTLPWhenUnset(t *testing.T) {
	// Not parallel: it sets the environment.
	c := newCollector(t)
	// Were the exporter made without an endpoint of Pisati's own, these
	// would send it to the collector.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.url)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", c.url+"/v1/traces")

	p := newProviders(t, config.Telemetry{})
	mux := http.NewServeMux()
	route(mux, "GET /p/{id}", ok)
	h := httpx.Chain(mux, p.HTTP(steppingClock(time.Millisecond), nil))
	if got := serve(t, h, http.MethodGet, "/p/1"); got != http.StatusOK {
		t.Fatalf("GET /p/1: status %d, want 200", got)
	}
	// Shutting down flushes whatever spans are waiting.
	shutdown(t, p)

	if got := c.requests(); len(got) != 0 {
		t.Errorf("with no OTLP endpoint set, %d requests reached a collector", len(got))
	}
}

func TestOTLPExportsSpan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, path, want string
	}{
		{name: "base URL", path: "", want: "/v1/traces"},
		{name: "base URL with a path", path: "/otlp/", want: "/otlp/v1/traces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newCollector(t)
			p := newProviders(t, config.Telemetry{OTLPEndpoint: endpoint(t, c.url+tt.path)})
			mux := http.NewServeMux()
			route(mux, "GET /p/{id}", ok)
			route(mux, "GET /boom", func(http.ResponseWriter, *http.Request) { panic("quokka") })
			route(mux, "GET /busy", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
			h := httpx.Chain(mux, httpx.Recover(slog.New(slog.DiscardHandler)), p.HTTP(steppingClock(250*time.Millisecond), nil))
			serve(t, h, http.MethodGet, "/p/123?q=quokka-query")
			serve(t, h, http.MethodGet, "/boom")
			serve(t, h, http.MethodGet, "/busy")
			serve(t, h, http.MethodGet, "/quokka-nowhere")
			shutdown(t, p)

			reqs := c.requests()
			if len(reqs) == 0 {
				t.Fatal("no request reached the collector")
			}
			spans := map[string]*tracepb.Span{}
			for _, req := range reqs {
				if req.path != tt.want {
					t.Errorf("the spans went to %s, want %s", req.path, tt.want)
				}
				for _, leak := range []string{"/p/123", "quokka-query", "quokka-cookie", "203.0.113.7", "probe/1.0", "quokka-nowhere"} {
					if bytes.Contains(req.body, []byte(leak)) {
						t.Errorf("the export carries %q of a request", leak)
					}
				}
				for _, rs := range decode(t, req.body).GetResourceSpans() {
					assertResource(t, rs)
					for _, ss := range rs.GetScopeSpans() {
						for _, s := range ss.GetSpans() {
							spans[s.GetName()] = s
						}
					}
				}
			}
			if len(spans) != 4 {
				t.Fatalf("spans %v, want GET /p/{id}, GET /boom, GET /busy and GET", slices.Sorted(maps.Keys(spans)))
			}

			for _, want := range []struct {
				name, route, status string
				failed              bool
			}{
				{"GET /p/{id}", "/p/{id}", "200", false},
				{"GET /boom", "/boom", "500", true},
				{"GET /busy", "/busy", "503", true},
				{"GET", "unmatched", "404", false},
			} {
				s, found := spans[want.name]
				if !found {
					t.Errorf("no span %q among %v", want.name, slices.Sorted(maps.Keys(spans)))
					continue
				}
				if len(s.GetParentSpanId()) != 0 || hex.EncodeToString(s.GetTraceId()) == clientTrace {
					t.Errorf("%s: continues the trace the client named", want.name)
				}
				if s.GetKind() != tracepb.Span_SPAN_KIND_SERVER {
					t.Errorf("%s: kind %v, want server", want.name, s.GetKind())
				}
				wantAttrs := map[string]string{
					"http.request.method":       "GET",
					"http.route":                want.route,
					"http.response.status_code": want.status,
				}
				if got := attributes(s.GetAttributes()); !maps.Equal(got, wantAttrs) {
					t.Errorf("%s: attributes %v, want exactly %v", want.name, got, wantAttrs)
				}
				if got := s.GetEndTimeUnixNano() - s.GetStartTimeUnixNano(); got != uint64(250*time.Millisecond) {
					t.Errorf("%s: lasted %d ns by the span, want 250ms by the injected clock", want.name, got)
				}
				if failed := s.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR; failed != want.failed {
					t.Errorf("%s: status %v, want an error: %t", want.name, s.GetStatus(), want.failed)
				}
			}
		})
	}
}

func TestHTTPContinuesTracesFromTrustedProxiesAlone(t *testing.T) {
	t.Parallel()

	// clientPeer is in this prefix, and not in the other.
	proxies := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	elsewhere := []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	sampled := "00-" + clientTrace + "-" + clientSpan + "-01"
	tests := []struct {
		name        string
		trusted     []netip.Prefix
		traceparent string
		// continued is whether the span joins the trace the request names,
		// and exported whether it is sent at all.
		continued, exported bool
	}{
		{name: "a trusted proxy's trace", trusted: proxies, traceparent: sampled, continued: true, exported: true},
		{name: "an untrusted peer's trace", trusted: nil, traceparent: sampled, exported: true},
		{name: "a peer outside the trusted prefixes", trusted: elsewhere, traceparent: sampled, exported: true},
		{name: "a trusted proxy's malformed traceparent", trusted: proxies, traceparent: "00-" + clientTrace + "-quokka-01", exported: true},
		{name: "a trusted proxy's traceparent of an unknown form", trusted: proxies, traceparent: "ff-" + clientTrace + "-" + clientSpan + "-01", exported: true},
		{name: "a trusted proxy with no traceparent", trusted: proxies, traceparent: "", exported: true},
		{name: "a trusted proxy's unsampled trace is not sampled", trusted: proxies, traceparent: "00-" + clientTrace + "-" + clientSpan + "-00", exported: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newCollector(t)
			p := newProviders(t, config.Telemetry{OTLPEndpoint: endpoint(t, c.url)})
			mux := http.NewServeMux()
			route(mux, "GET /p/{id}", ok)
			h := httpx.Chain(mux, p.HTTP(steppingClock(time.Millisecond), tt.trusted))
			req := request(t, http.MethodGet, "/p/1")
			req.Header.Del("Traceparent")
			if tt.traceparent != "" {
				req.Header.Set("Traceparent", tt.traceparent)
			}
			req.Header.Set("Tracestate", "quokka=1")
			// The proxy names the client it forwards for, which no prefix
			// holds: trust is the peer's, not the client's.
			req.Header.Set("X-Forwarded-For", "192.0.2.55")
			h.ServeHTTP(httptest.NewRecorder(), req)
			shutdown(t, p)

			var spans []*tracepb.Span
			for _, r := range c.requests() {
				for _, rs := range decode(t, r.body).GetResourceSpans() {
					for _, ss := range rs.GetScopeSpans() {
						spans = append(spans, ss.GetSpans()...)
					}
				}
			}
			if !tt.exported {
				if len(spans) != 0 {
					t.Errorf("%d spans exported, want none", len(spans))
				}
				return
			}
			if len(spans) != 1 {
				t.Fatalf("%d spans exported, want 1", len(spans))
			}
			s := spans[0]
			trace, parent := hex.EncodeToString(s.GetTraceId()), hex.EncodeToString(s.GetParentSpanId())
			if tt.continued {
				if trace != clientTrace || parent != clientSpan || s.GetTraceState() != "quokka=1" {
					t.Errorf("trace %s, parent %q, state %q; want the request's: %s, %s, quokka=1",
						trace, parent, s.GetTraceState(), clientTrace, clientSpan)
				}
				return
			}
			if trace == clientTrace || parent != "" || s.GetTraceState() != "" {
				t.Errorf("trace %s, parent %q, state %q; want a fresh trace", trace, parent, s.GetTraceState())
			}
		})
	}
}

func TestTelemetryIgnoresOTELEnvironment(t *testing.T) {
	// Not parallel: it sets the environment.
	ours, theirs := newCollector(t), newCollector(t)
	for name, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":        theirs.url,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": theirs.url + "/v1/traces",
		"OTEL_EXPORTER_OTLP_HEADERS":         "x-from-env=from-env",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":  "x-traces-from-env=from-env",
		"OTEL_EXPORTER_OTLP_PROTOCOL":        "http/json",
		"OTEL_EXPORTER_OTLP_COMPRESSION":     "gzip",
		"OTEL_EXPORTER_OTLP_TIMEOUT":         "1",
		// A CA for an http endpoint would make the exporter refuse to start.
		"OTEL_EXPORTER_OTLP_CERTIFICATE":  caFile(t),
		"OTEL_RESOURCE_ATTRIBUTES":        "from.env=from-env,service.version=from-env",
		"OTEL_SERVICE_NAME":               "from-env",
		"OTEL_TRACES_SAMPLER":             "always_off",
		"OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT": "1",
		"OTEL_GO_X_CARDINALITY_LIMIT":     "1",
		// No option overrides these: neither may change what is exported.
		"OTEL_GO_X_OBSERVABILITY":               "true",
		"OTEL_GO_X_PER_SERIES_START_TIMESTAMPS": "true",
	} {
		t.Setenv(name, value)
	}

	p := newProviders(t, config.Telemetry{OTLPEndpoint: endpoint(t, ours.url)})
	mux := http.NewServeMux()
	route(mux, "GET /p/{id}", ok)
	route(mux, "GET /q/{id}", ok)
	h := httpx.Chain(mux, p.HTTP(steppingClock(time.Millisecond), nil))
	serve(t, h, http.MethodGet, "/p/1")
	serve(t, h, http.MethodGet, "/q/1")

	text := scrape(t, p)
	for _, leak := range []string{"from-env", "from_env", "otel_metric_overflow"} {
		if strings.Contains(text, leak) {
			t.Errorf("/metrics shows %q, from the environment:\n%s", leak, text)
		}
	}
	all := samples(t, text)
	for _, s := range all {
		if !strings.HasPrefix(s.name, duration+"_") {
			t.Errorf("/metrics serves %s, want the request histogram alone", s.name)
		}
	}
	for _, r := range []string{"/p/{id}", "/q/{id}"} {
		if got := value(t, all, duration+"_count", map[string]string{"http_route": r}); got != "1" {
			t.Errorf("route %s: count %s, want 1", r, got)
		}
	}

	shutdown(t, p)
	if got := theirs.requests(); len(got) != 0 {
		t.Errorf("%d requests went to the collector the environment names", len(got))
	}
	reqs := ours.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the configured collector")
	}
	spans := 0
	for _, req := range reqs {
		if req.path != "/v1/traces" {
			t.Errorf("the spans went to %s, want /v1/traces", req.path)
		}
		for name := range req.header {
			if strings.Contains(strings.ToLower(name), "from-env") {
				t.Errorf("the export carries the header %s, from the environment", name)
			}
		}
		if got := req.header.Get("Content-Type"); got != "application/x-protobuf" {
			t.Errorf("Content-Type %q, want application/x-protobuf", got)
		}
		if got := req.header.Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding %q, want none", got)
		}
		if bytes.Contains(req.body, []byte("from-env")) {
			t.Error("the export carries a value from the environment")
		}
		for _, rs := range decode(t, req.body).GetResourceSpans() {
			assertResource(t, rs)
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					spans++
					if n := len(s.GetAttributes()); n != 3 {
						t.Errorf("%s: %d attributes, want 3", s.GetName(), n)
					}
				}
			}
		}
	}
	if spans != 2 {
		t.Errorf("%d spans exported, want 2", spans)
	}
}

func TestFailedExportIsLoggedOnce(t *testing.T) {
	t.Parallel()

	// bound is the most of the collector's answer the exporter reads.
	const bound = 64 << 10
	tests := []struct {
		name string
		rep  reply
		// want is in the logged error.
		want string
	}{
		{
			name: "a refusal",
			rep:  reply{status: http.StatusBadRequest, body: []byte("quokka refused")},
			want: "400 Bad Request (body: quokka refused)",
		},
		{
			name: "a refusal longer than the bound",
			rep:  reply{status: http.StatusBadRequest, body: bytes.Repeat([]byte("quokka "), 1<<20/7)},
			want: "response body too large: exceeded 65536 bytes",
		},
		{
			name: "a header longer than the bound",
			rep:  reply{status: http.StatusOK, header: map[string]string{"X-Quokka": strings.Repeat("q", 2*bound)}},
			want: "server response headers exceeded 65536 bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newReplyingCollector(t, tt.rep)
			log, buf := newLog()
			p := newLoggingProviders(t, config.Telemetry{OTLPEndpoint: endpoint(t, c.url)}, log)
			mux := http.NewServeMux()
			route(mux, "GET /p/{id}", ok)
			serve(t, httpx.Chain(mux, p.HTTP(steppingClock(time.Millisecond), nil)), http.MethodGet, "/p/1")
			shutdown(t, p)

			recs := buf.records(t)
			if len(recs) != 1 {
				t.Fatalf("%d records, want one: %v", len(recs), recs)
			}
			rec := recs[0]
			if rec["level"] != "WARN" || rec["msg"] != "trace export failed" || rec["spans"] != 1.0 {
				t.Errorf("the record %v, want level WARN, msg \"trace export failed\", spans 1", rec)
			}
			text, isText := rec["error"].(string)
			if !isText {
				t.Fatalf("the record %v has no error text", rec)
			}
			if !strings.Contains(text, tt.want) {
				t.Errorf("the error %.300q has no %q", text, tt.want)
			}
			if len(text) > bound {
				t.Errorf("the error is %d bytes, over the %d-byte bound", len(text), bound)
			}
		})
	}
}

func TestNewProvidersRefuses(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	tests := []struct {
		name    string
		version string
		log     *slog.Logger
	}{
		{name: "no version", version: "", log: log},
		{name: "no logger", version: testVersion, log: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := telemetry.NewProviders(t.Context(), config.Telemetry{}, tt.version, tt.log)
			if !errors.Is(err, telemetry.ErrInvalid) {
				t.Errorf("NewProviders: %v, want ErrInvalid", err)
			}
			if err := p.Shutdown(t.Context()); err != nil {
				t.Errorf("Shutdown of the Providers returned with the error: %v", err)
			}
		})
	}
}

func TestShutdownWithAnEndedContext(t *testing.T) {
	// Not parallel: it checks that every goroutine it starts ends, and
	// another test's would get in the way.
	log, buf := newLog()
	// Cleanups run last first: the log is read once the goroutines end.
	t.Cleanup(func() {
		if recs := buf.records(t); len(recs) != 0 {
			t.Errorf("logged %v: the end of ctx is Shutdown's to report, and nothing failed", recs)
		}
	})
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, before) })

	c := newCollector(t)
	p := newLoggingProviders(t, config.Telemetry{OTLPEndpoint: endpoint(t, c.url)}, log)
	mux := http.NewServeMux()
	route(mux, "GET /p/{id}", ok)
	h := httpx.Chain(mux, p.HTTP(steppingClock(time.Millisecond), nil))
	serve(t, h, http.MethodGet, "/p/1")

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.Shutdown(ended); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown: %v, want nil or context.Canceled", err)
	}
	// The batcher stops all the same, sending the span that was waiting.
	c.wait(t, 1)
	if text := scrape(t, p); strings.Contains(text, duration) {
		t.Errorf("/metrics still serves readings after Shutdown:\n%s", text)
	}
}

func TestShutdownTwice(t *testing.T) {
	t.Parallel()

	c := newCollector(t)
	p := newProviders(t, config.Telemetry{OTLPEndpoint: endpoint(t, c.url)})
	shutdown(t, p)
	shutdown(t, p)
}

// decode returns body as the request an OTLP/HTTP exporter sends,
// failing the test if it is not one.
func decode(t *testing.T, body []byte) *coltracepb.ExportTraceServiceRequest {
	t.Helper()
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		t.Fatalf("the export is not an OTLP protobuf request: %v", err)
	}
	return &req
}

// assertResource fails the test unless rs's resource is exactly Pisati's.
func assertResource(t *testing.T, rs *tracepb.ResourceSpans) {
	t.Helper()
	want := map[string]string{"service.name": "pisati", "service.version": testVersion}
	if got := attributes(rs.GetResource().GetAttributes()); !maps.Equal(got, want) {
		t.Errorf("resource %v, want exactly %v", got, want)
	}
}

// attributes returns kvs as a map, each value as text.
func attributes(kvs []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			out[kv.GetKey()] = v.StringValue
		case *commonpb.AnyValue_IntValue:
			out[kv.GetKey()] = strconv.FormatInt(v.IntValue, 10)
		default:
			out[kv.GetKey()] = "a value of another type"
		}
	}
	return out
}

// caFile writes a CA certificate, in PEM, to a file of the test's own, and
// returns its path.
func caFile(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	cert := srv.Certificate()
	srv.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatalf("writing the CA: %v", err)
	}
	return path
}
