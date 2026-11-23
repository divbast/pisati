// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package telemetry reports what Pisati does. NewLogger makes the
// structured logger. NewProviders sets up the OpenTelemetry SDK: metrics,
// which Metrics serves in Prometheus's format for the internal listener's
// /metrics, and traces, which go out over OTLP/HTTP only when an endpoint
// is configured. HTTP is the middleware that records each request by its
// route pattern, which RecordRoute reports from inside the mux, and Pprof
// mounts the profiler when it is on.
//
// Nothing here is global: no default logger, tracer or meter provider,
// propagator or Prometheus registry is set or used. Nor does anything come
// from the environment. The SDK reads OTEL_* variables, so every setting
// one of them would change is set here explicitly, and Pisati is
// configured through its own settings alone. The SDK's experimental
// OTEL_GO_X_ switches have no option to set them by; those it has change
// nothing Pisati exports.
package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/httpconv"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/divbast/pisati/internal/config"
)

// ErrInvalid is an argument NewProviders refuses.
var ErrInvalid = errors.New("invalid")

const (
	// serviceName is the resource's service.name.
	serviceName = "pisati"
	// scope names the meter and the tracer, for this package.
	scope = "github.com/divbast/pisati/internal/telemetry"

	// otlpTimeout bounds each exchange with the collector.
	otlpTimeout = 10 * time.Second
	// otlpIdle is how long a connection to the collector stays open idle.
	otlpIdle = 90 * time.Second
	// collectorReply bounds the body of the collector's answer that the
	// exporter reads, which it puts in its error when the export fails.
	collectorReply = 64 << 10
	// collectorHeader bounds the header of the collector's answer, as the
	// guarded client bounds the header of every answer it reads.
	collectorHeader = 64 << 10
	// attributeValue bounds a span attribute's value, in bytes, which the
	// SDK otherwise leaves unbounded.
	attributeValue = 1024

	// series bounds the series one instrument keeps: the SDK's default.
	series = 2000
	// scrapes bounds the scrapes of /metrics served at once; more are
	// answered 503.
	scrapes = 4
)

// Providers holds Pisati's meter and tracer providers. Metrics serves the
// meters' readings, HTTP records each request with both, and Shutdown
// stops them. A copy shares the providers.
//
// The zero Providers holds none: Metrics and HTTP panic on it, as a bug,
// and its Shutdown does nothing.
type Providers struct {
	meters   *sdkmetric.MeterProvider
	metrics  http.Handler
	duration httpconv.ServerRequestDuration
	// traces is the zero tracing when no OTLP endpoint is configured, and
	// tracer is then a no-op.
	traces tracing
	tracer trace.Tracer
	// propagator reads the W3C trace headers of a trusted proxy's request.
	propagator propagation.TraceContext
}

// tracing is the tracer provider spans are made by, and the batcher that
// sends them. The zero tracing makes and sends none.
type tracing struct {
	provider *sdktrace.TracerProvider
	batcher  sdktrace.SpanProcessor
}

// NewProviders returns Providers that describe this process as
// service.name pisati at service.version version, and as nothing else.
//
// Metrics go to a Prometheus registry of the Providers' own, which Metrics
// serves. Traces are exported only when c.OTLPEndpoint is set; otherwise
// the tracer is a no-op and no exporter is made, so no span is sent
// anywhere unless Pisati's own setting says where. The endpoint is a
// collector's base URL, as OTEL_EXPORTER_OTLP_ENDPOINT is: spans go to its
// path joined with /v1/traces, as uncompressed protobuf, with no header of
// Pisati's, each exchange within 10 s. Ended spans wait in a bounded queue
// and go out in batches; when the queue is full, new spans are dropped,
// and a request never waits for an export. An export the collector turns
// away for now is retried, for at most 30 s; one that still fails is
// logged to log at warn level, once, and its spans are dropped.
//
// No OTEL_* variable changes any of this.
//
// It returns an error wrapping ErrInvalid for an empty version or a nil
// log, ctx's error when ctx has ended, and otherwise any error the SDK
// returns in setting up.
func NewProviders(ctx context.Context, c config.Telemetry, version string, log *slog.Logger) (Providers, error) {
	var errs []error
	if version == "" {
		errs = append(errs, fmt.Errorf("telemetry: %w version: empty", ErrInvalid))
	}
	if log == nil {
		errs = append(errs, fmt.Errorf("telemetry: %w logger: nil", ErrInvalid))
	}
	if err := errors.Join(errs...); err != nil {
		return Providers{}, err
	}
	if err := ctx.Err(); err != nil {
		return Providers{}, err
	}

	// Built here, never from resource.Default, which reads the environment.
	res := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(serviceName), semconv.ServiceVersion(version))
	// Until newTraces has made its batcher, nothing made here runs or holds
	// a connection, so an error below leaves nothing to stop.
	meters, metrics, err := newMeters(res)
	if err != nil {
		return Providers{}, err
	}
	duration, err := httpconv.NewServerRequestDuration(meters.Meter(scope))
	if err != nil {
		return Providers{}, fmt.Errorf("telemetry: the request duration histogram: %w", err)
	}
	p := Providers{
		meters:   meters,
		metrics:  metrics,
		duration: duration,
		tracer:   noop.NewTracerProvider().Tracer(scope),
	}
	if u, ok := c.OTLPEndpoint.Get(); ok {
		if p.traces, err = newTraces(ctx, u, res, log); err != nil {
			return Providers{}, err
		}
		p.tracer = p.traces.provider.Tracer(scope)
	}
	return p, nil
}

// Shutdown stops both providers, and returns their failures joined. Both
// stop even when ctx has already ended: nothing HTTP records afterwards is
// kept, and Metrics then serves no readings.
//
// The spans still waiting are sent first, in one last export, which the
// batcher's own 30 s export timeout bounds. Shutdown waits for that export
// until ctx ends; if ctx ends first, it returns ctx's error while the
// export carries on in the background, its goroutine ending with it.
// Calls after the first do nothing and return nil.
func (p Providers) Shutdown(ctx context.Context) error {
	if p.meters == nil {
		return nil
	}
	var errs []error
	if p.traces.provider != nil {
		// The batcher first: the tracer provider stops no span processor
		// once ctx has ended, and stopping it afterwards does nothing.
		errs = append(errs, p.traces.batcher.Shutdown(ctx), p.traces.provider.Shutdown(ctx))
	}
	// The meter provider stops its reader whatever ctx says, and reports
	// ErrReaderShutdown on every call after the first.
	if err := p.meters.Shutdown(ctx); !errors.Is(err, sdkmetric.ErrReaderShutdown) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// made panics, as a bug, on the zero Providers.
func (p Providers) made() {
	if p.meters == nil {
		panic("telemetry: Providers not made by NewProviders")
	}
}

// newMeters returns a meter provider, and the handler that serves its
// readings from a Prometheus registry of their own.
func newMeters(res *resource.Resource) (*sdkmetric.MeterProvider, http.Handler, error) {
	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		// target_info would show the resource, into which the SDK merges
		// OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME with no way to
		// stop it.
		otelprom.WithoutTargetInfo(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: the Prometheus exporter: %w", err)
	}
	meters := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(exp),
		// At the SDK's defaults, set so that no OTEL_ variable changes them.
		sdkmetric.WithExemplarFilter(exemplar.TraceBasedFilter),
		sdkmetric.WithCardinalityLimit(series),
	)
	return meters, promhttp.HandlerFor(reg, promhttp.HandlerOpts{MaxRequestsInFlight: scrapes}), nil
}

// newTraces returns a tracer provider whose spans, once ended, its batcher
// sends in batches to the OTLP/HTTP collector whose base URL is base.
func newTraces(ctx context.Context, base url.URL, res *resource.Resource, log *slog.Logger) (tracing, error) {
	base.Path, base.RawPath = path.Join(base.Path, "/v1/traces"), ""
	client := otlpClient()
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(base.String()),
		otlptracehttp.WithHTTPClient(client),
		// Every option the SDK would otherwise take from an OTEL_ variable:
		// an option overrides the environment. The client above overrides
		// the timeout and TLS settings too, but they are set all the same.
		otlptracehttp.WithHeaders(map[string]string{}),
		otlptracehttp.WithEncoding(otlptracehttp.EncodingProtobuf),
		otlptracehttp.WithCompression(otlptracehttp.NoCompression),
		otlptracehttp.WithTimeout(otlpTimeout),
		otlptracehttp.WithTLSClientConfig(nil),
		otlptracehttp.WithMaxResponseSize(collectorReply),
	)
	if err != nil {
		return tracing{}, fmt.Errorf("telemetry: the OTLP exporter: %w", err)
	}
	// Below, every setting an OTEL_ variable would change, at the SDK's
	// default but for the attribute value bound.
	batcher := sdktrace.NewBatchSpanProcessor(spanExporter{next: exp, res: res, log: log, client: client},
		sdktrace.WithBatchTimeout(sdktrace.DefaultScheduleDelay*time.Millisecond),
		sdktrace.WithExportTimeout(sdktrace.DefaultExportTimeout*time.Millisecond),
		sdktrace.WithMaxQueueSize(sdktrace.DefaultMaxQueueSize),
		sdktrace.WithMaxExportBatchSize(sdktrace.DefaultMaxExportBatchSize),
	)
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(batcher),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{
			AttributeValueLengthLimit:   attributeValue,
			AttributeValueDepthLimit:    sdktrace.DefaultAttributeValueDepthLimit,
			AttributeCountLimit:         sdktrace.DefaultAttributeCountLimit,
			EventCountLimit:             sdktrace.DefaultEventCountLimit,
			LinkCountLimit:              sdktrace.DefaultLinkCountLimit,
			AttributePerEventCountLimit: sdktrace.DefaultAttributePerEventCountLimit,
			AttributePerLinkCountLimit:  sdktrace.DefaultAttributePerLinkCountLimit,
		}),
		sdktrace.WithIDGenerator(randomIDs{}),
	)
	return tracing{provider: provider, batcher: batcher}, nil
}

// otlpClient returns the client spans go out on. The collector is the
// operator's own, often at a private address, which the guarded client
// refuses, so this client checks no address. It is bounded as that one
// is: each exchange, the TLS handshake included, ends within otlpTimeout,
// and the answer's header is read to collectorHeader at most, as its body
// is, by the exporter, to collectorReply. It follows no redirect, where
// the guarded client follows a few, since the collector is where the
// operator said.
func otlpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// Never http.ProxyFromEnvironment: the environment is main's to
			// read.
			Proxy: nil,
			// The transport bounds a handshake only when told to.
			TLSHandshakeTimeout:    otlpTimeout,
			MaxResponseHeaderBytes: collectorHeader,
			IdleConnTimeout:        otlpIdle,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       otlpTimeout,
	}
}

// spanExporter exports spans through next, under res alone, and logs a
// failed export to log. The SDK merges OTEL_RESOURCE_ATTRIBUTES and
// OTEL_SERVICE_NAME into every resource it is given, with no option to
// stop it, so each span is given res back here. A failure logged here,
// with no error handed back to the SDK, is handled once, and stays out of
// OTel's global error handler, which writes unstructured lines to stderr.
type spanExporter struct {
	next   sdktrace.SpanExporter
	res    *resource.Resource
	log    *slog.Logger
	client *http.Client
}

func (e spanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = withResource{ReadOnlySpan: s, res: e.res}
	}
	if err := e.next.ExportSpans(ctx, out); err != nil {
		e.log.WarnContext(ctx, "trace export failed", "spans", len(spans), "error", err)
	}
	return nil
}

// Shutdown stops next, then closes the client's idle connections, each of
// which keeps goroutines of its own until it is closed. The SDK does not
// hand an error from here to the caller of Providers.Shutdown, so it is
// logged here instead: all but ctx's end, which that caller is told of.
func (e spanExporter) Shutdown(ctx context.Context) error {
	if err := e.next.Shutdown(ctx); err != nil && (ctx.Err() == nil || !errors.Is(err, ctx.Err())) {
		e.log.WarnContext(ctx, "trace exporter shutdown failed", "error", err)
	}
	e.client.CloseIdleConnections()
	return nil
}

// withResource is an ended span, under res.
type withResource struct {
	sdktrace.ReadOnlySpan

	res *resource.Resource
}

func (s withResource) Resource() *resource.Resource {
	return s.res
}

// randomIDs makes trace and span IDs from crypto/rand, where the SDK's own
// generator uses math/rand.
type randomIDs struct{}

func (randomIDs) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	var id trace.TraceID
	for !id.IsValid() {
		// Read never returns an error: it crashes the program instead.
		rand.Read(id[:])
	}
	return id, newSpanID()
}

func (randomIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	return newSpanID()
}

// newSpanID returns a span ID from crypto/rand.
func newSpanID() trace.SpanID {
	var id trace.SpanID
	for !id.IsValid() {
		// Read never returns an error: it crashes the program instead.
		rand.Read(id[:])
	}
	return id
}
