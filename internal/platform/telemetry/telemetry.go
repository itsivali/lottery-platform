// Package telemetry wires OpenTelemetry traces and metrics, Prometheus exposition and trace-correlated slog logs.
//
// Traces go to an OTLP/HTTP endpoint when one is configured (Grafana Cloud or a self-hosted collector) and are
// otherwise recorded but not exported. Metrics are always exposed in Prometheus format for scraping.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

// Config selects what to export.
type Config struct {
	ServiceName string
	// OTLPEndpoint is the OTLP/HTTP traces endpoint host:port ("" disables trace export).
	OTLPEndpoint string
	// OTLPInsecure disables TLS to the endpoint (local collectors only).
	OTLPInsecure bool
	// SpanProcessors are extra processors (tests use an in-memory recorder).
	SpanProcessors []sdktrace.SpanProcessor
}

// Telemetry owns the providers.
type Telemetry struct {
	tp  *sdktrace.TracerProvider
	mp  *sdkmetric.MeterProvider
	reg *prometheus.Registry
}

// New builds the providers. Nothing is installed globally, so tests and multiple instances stay isolated.
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(cfg.ServiceName)))
	if err != nil {
		return nil, err
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if cfg.OTLPEndpoint != "" {
		eo := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.OTLPInsecure {
			eo = append(eo, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, eo...)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	for _, sp := range cfg.SpanProcessors {
		opts = append(opts, sdktrace.WithSpanProcessor(sp))
	}
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, err
	}
	return &Telemetry{
		tp:  sdktrace.NewTracerProvider(opts...),
		mp:  sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(exporter)),
		reg: reg,
	}, nil
}

// Shutdown flushes and stops the providers.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	return errors.Join(t.tp.Shutdown(ctx), t.mp.Shutdown(ctx))
}

// Middleware traces and measures every HTTP request.
func (t *Telemetry) Middleware(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "http",
		otelhttp.WithTracerProvider(t.tp), otelhttp.WithMeterProvider(t.mp),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }))
}

// MetricsHandler serves the Prometheus exposition. Mount it on an internal listener, never the public port.
func (t *Telemetry) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(t.reg, promhttp.HandlerOpts{})
}

// logHandler adds trace_id and span_id to records logged inside a span.
type logHandler struct{ slog.Handler }

// NewLogHandler wraps h so log lines carry the active trace and span ids.
func NewLogHandler(h slog.Handler) slog.Handler { return logHandler{h} }

func (l logHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return l.Handler.Handle(ctx, r)
}

func (l logHandler) WithAttrs(a []slog.Attr) slog.Handler { return logHandler{l.Handler.WithAttrs(a)} }
func (l logHandler) WithGroup(n string) slog.Handler      { return logHandler{l.Handler.WithGroup(n)} }
