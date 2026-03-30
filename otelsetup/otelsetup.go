// Package otelsetup initializes OpenTelemetry providers (trace, metric, log)
// via OTLP/HTTP exporters and registers them as OTel globals.
//
//	shutdown, err := otelsetup.Initialize(ctx,
//	    otelsetup.WithServiceName("my-svc"),
//	    otelsetup.WithOtelCollectorUri("otelcollector:4318"),
//	)
//	defer shutdown(ctx)
package otelsetup

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// ── Options ─────────────────────────────────────────────────────────────────

type config struct {
	name     string
	endpoint string
}

// Option configures otelsetup.
type Option func(*config)

// WithName sets the instrumentation name (same effect as WithServiceName).
func WithName(n string) Option { return func(c *config) { c.name = n } }

// WithServiceName sets the service.name attribute in the OTel resource.
func WithServiceName(n string) Option { return func(c *config) { c.name = n } }

// WithOtelCollectorUri sets the OTLP collector endpoint (e.g. "localhost:4318").
func WithOtelCollectorUri(u string) Option { return func(c *config) { c.endpoint = u } }

// ── Providers ───────────────────────────────────────────────────────────────

// Providers groups the three OTel SDK providers and a combined shutdown function.
type Providers struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider
	// Shutdown gracefully shuts down all providers.
	Shutdown func(context.Context) error
}

// Initialize creates OTLP/HTTP providers, registers them as OTel globals
// (TracerProvider, MeterProvider, LoggerProvider and TextMapPropagator) and
// returns a shutdown function.
func Initialize(ctx context.Context, opts ...Option) (func(context.Context) error, error) {
	cfg := applyOpts(opts)
	p, err := build(ctx, cfg)
	if err != nil {
		return p.Shutdown, err
	}

	otel.SetTracerProvider(p.TracerProvider)
	otel.SetMeterProvider(p.MeterProvider)
	global.SetLoggerProvider(p.LoggerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return p.Shutdown, nil
}

// ── internal ─────────────────────────────────────────────────────────────────

func applyOpts(opts []Option) *config {
	cfg := &config{name: "unknown", endpoint: "localhost:4318"}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

func build(ctx context.Context, cfg *config) (*Providers, error) {
	res, _ := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(cfg.name),
		),
	)

	// ── Trace ───────────────────────────────────────────────────────────────
	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithEndpoint(cfg.endpoint),
	)
	if err != nil {
		return noopShutdown(), err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// ── Metric ──────────────────────────────────────────────────────────────
	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithInsecure(),
		otlpmetrichttp.WithEndpoint(cfg.endpoint),
	)
	if err != nil {
		return &Providers{Shutdown: tp.Shutdown}, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(30*time.Second),
		)),
		sdkmetric.WithResource(res),
	)

	// ── Log ─────────────────────────────────────────────────────────────────
	logExp, err := otlploghttp.New(ctx,
		otlploghttp.WithInsecure(),
		otlploghttp.WithEndpoint(cfg.endpoint),
	)
	if err != nil {
		return &Providers{
			Shutdown: func(ctx context.Context) error {
				return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
			},
		}, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)

	return &Providers{
		TracerProvider: tp,
		MeterProvider:  mp,
		LoggerProvider: lp,
		Shutdown: func(ctx context.Context) error {
			return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
		},
	}, nil
}

func noopShutdown() *Providers {
	return &Providers{Shutdown: func(context.Context) error { return nil }}
}
