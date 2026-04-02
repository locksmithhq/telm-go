// Package otelsetup initializes OpenTelemetry providers (trace, metric, log)
// via OTLP/HTTP or OTLP/gRPC exporters and registers them as OTel globals.
//
// HTTP (default):
//
//	shutdown, err := otelsetup.Initialize(ctx,
//	    otelsetup.WithServiceName("my-svc"),
//	    otelsetup.WithOtelCollectorUri("localhost:4318"),
//	)
//
// gRPC:
//
//	shutdown, err := otelsetup.Initialize(ctx,
//	    otelsetup.WithServiceName("my-svc"),
//	    otelsetup.WithOtelCollectorUri("localhost:4317"),
//	    otelsetup.WithGRPC(),
//	)
package otelsetup

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ── Options ──────────────────────────────────────────────────────────────────

type config struct {
	name     string
	endpoint string
	grpc     bool
	headers  map[string]string
}

// Option configures otelsetup.
type Option func(*config)

// WithName sets the instrumentation name (same effect as WithServiceName).
func WithName(n string) Option { return func(c *config) { c.name = n } }

// WithServiceName sets the service.name attribute in the OTel resource.
func WithServiceName(n string) Option { return func(c *config) { c.name = n } }

// WithOtelCollectorUri sets the OTLP collector endpoint.
// For HTTP: "localhost:4318" or "telm.example.com/otlp"
// For gRPC: "localhost:4317"
func WithOtelCollectorUri(u string) Option { return func(c *config) { c.endpoint = u } }

// WithGRPC switches the exporter protocol from HTTP to gRPC.
func WithGRPC() Option { return func(c *config) { c.grpc = true } }

// WithHeaders adds extra headers to every export request.
// For HTTP: sent as HTTP headers (e.g. "X-API-Key").
// For gRPC: sent as metadata.
func WithHeaders(h map[string]string) Option { return func(c *config) { c.headers = h } }

// ── Providers ────────────────────────────────────────────────────────────────

// Providers groups the three OTel SDK providers and a combined shutdown function.
type Providers struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider
	Shutdown       func(context.Context) error
}

// Initialize creates OTLP providers, registers them as OTel globals and
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

// ── internal ──────────────────────────────────────────────────────────────────

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

	if cfg.grpc {
		return buildGRPC(ctx, cfg, res)
	}
	return buildHTTP(ctx, cfg, res)
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

func buildHTTP(ctx context.Context, cfg *config, res *resource.Resource) (*Providers, error) {
	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithEndpoint(cfg.endpoint),
		otlptracehttp.WithHeaders(cfg.headers),
	)
	if err != nil {
		return noopShutdown(), err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithInsecure(),
		otlpmetrichttp.WithEndpoint(cfg.endpoint),
		otlpmetrichttp.WithHeaders(cfg.headers),
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

	logExp, err := otlploghttp.New(ctx,
		otlploghttp.WithInsecure(),
		otlploghttp.WithEndpoint(cfg.endpoint),
		otlploghttp.WithHeaders(cfg.headers),
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

// ── gRPC ──────────────────────────────────────────────────────────────────────

func buildGRPC(ctx context.Context, cfg *config, res *resource.Resource) (*Providers, error) {
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if len(cfg.headers) > 0 {
		md := metadata.New(cfg.headers)
		dialOpts = append(dialOpts, grpc.WithUnaryInterceptor(metadataInterceptor(md)))
		dialOpts = append(dialOpts, grpc.WithStreamInterceptor(metadataStreamInterceptor(md)))
	}

	conn, err := grpc.NewClient(cfg.endpoint, dialOpts...)
	if err != nil {
		return noopShutdown(), err
	}

	traceExp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return noopShutdown(), err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	metricExp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return &Providers{Shutdown: tp.Shutdown}, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(30*time.Second),
		)),
		sdkmetric.WithResource(res),
	)

	logExp, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
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
			return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx), conn.Close())
		},
	}, nil
}

func metadataInterceptor(md metadata.MD) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.NewOutgoingContext(ctx, md)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func metadataStreamInterceptor(md metadata.MD) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.NewOutgoingContext(ctx, md)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func noopShutdown() *Providers {
	return &Providers{Shutdown: func(context.Context) error { return nil }}
}
