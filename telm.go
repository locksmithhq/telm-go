// Package telm is the observability SDK.
// Provides a minimal, ergonomic API for traces, logs and metrics.
//
// Basic usage:
//
//	shutdown, _ := telm.Init(ctx, telm.WithServiceName("my-svc"), telm.WithEndpoint("collector:4318"))
//	defer shutdown(ctx)
//
//	ctx, end := telm.Start(ctx, "operation")
//	defer end(nil)
//
//	telm.Info(ctx, "user fetched", telm.F{"id": user.ID})
//	telm.Count(ctx, "users.fetched", 1, telm.F{"region": "br"})
package telm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/locksmithhq/telm-go/otelsetup"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ── Public types ───────────────────────────────────────────────────────────

// F is a shorthand for log fields / metric attributes.
// Example: telm.F{"user_id": 42, "route": "/api/orders"}
type F map[string]any

// Level represents the log severity.
type Level int

const (
	DEBUG Level = iota
	INFO
	WARN
	ERROR
	FATAL
)

// ── Initialization ─────────────────────────────────────────────────────────

type config struct {
	serviceName string
	endpoint    string
	grpc        bool
	headers     map[string]string
}

var (
	globalCfg  *config
	cfgMu      sync.RWMutex
	counters   sync.Map // map[string]metric.Int64Counter
	histograms sync.Map // map[string]metric.Float64Histogram
	gauges     sync.Map // map[string]metric.Int64UpDownCounter
)

// Option configures the SDK.
type Option func(*config)

func WithServiceName(name string) Option { return func(c *config) { c.serviceName = name } }
func WithEndpoint(ep string) Option      { return func(c *config) { c.endpoint = ep } }

// WithGRPC switches the exporter protocol from HTTP (default) to gRPC.
// Use with endpoint "host:4317".
func WithGRPC() Option { return func(c *config) { c.grpc = true } }

// WithHeaders adds extra headers/metadata to every export request.
// For HTTP: use for "X-API-Key" when sending via /otlp proxy.
// For gRPC: sent as gRPC metadata on every call.
func WithHeaders(h map[string]string) Option { return func(c *config) { c.headers = h } }

// Init initializes the SDK and returns a shutdown function.
// Should be called once at application startup.
// After initialization, process and runtime metrics are collected automatically.
func Init(ctx context.Context, opts ...Option) (func(context.Context) error, error) {
	cfg := &config{serviceName: "unknown", endpoint: "localhost:4318"}
	for _, opt := range opts {
		opt(cfg)
	}
	cfgMu.Lock()
	globalCfg = cfg
	cfgMu.Unlock()

	setupOpts := []otelsetup.Option{
		otelsetup.WithServiceName(cfg.serviceName),
		otelsetup.WithOtelCollectorUri(cfg.endpoint),
	}
	if cfg.grpc {
		setupOpts = append(setupOpts, otelsetup.WithGRPC())
	}
	if len(cfg.headers) > 0 {
		setupOpts = append(setupOpts, otelsetup.WithHeaders(cfg.headers))
	}
	shutdown, err := otelsetup.Initialize(ctx, setupOpts...)
	if err != nil {
		return shutdown, err
	}

	registerRuntimeMetrics(otel.Meter(cfg.serviceName))
	return shutdown, nil
}

func svcName() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if globalCfg == nil {
		return "unknown"
	}
	return globalCfg.serviceName
}

// ── Tracing ────────────────────────────────────────────────────────────────

// Start opens a new span and returns the enriched context and an end(err) callback.
// Call end(nil) on success or end(err) on failure — the span is closed in both cases.
//
//	ctx, end := telm.Start(ctx, "db.query", telm.Server())
//	defer end(nil)
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, func(error)) {
	ctx, span := otel.Tracer(svcName()).Start(ctx, name, opts...)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// Attr adds attributes to the active span in context.
// Accepts one or more F maps — fields are merged before being applied.
//
//	telm.Attr(ctx, telm.F{"http.request.method": "GET", "http.route": "/api/users"})
func Attr(ctx context.Context, fields ...F) {
	trace.SpanFromContext(ctx).SetAttributes(toAttrs(mergeF(fields...))...)
}

// Event records an event on the active span with optional fields.
//
//	telm.Event(ctx, "cache.hit")
//	telm.Event(ctx, "row.fetched", telm.F{"rows": 1})
func Event(ctx context.Context, name string, fields ...F) {
	trace.SpanFromContext(ctx).AddEvent(name, trace.WithAttributes(toAttrs(mergeF(fields...))...))
}

// Server returns SpanKind = SERVER (incoming request entry point).
func Server() trace.SpanStartOption { return trace.WithSpanKind(trace.SpanKindServer) }

// Client returns SpanKind = CLIENT (outgoing call to external service).
func Client() trace.SpanStartOption { return trace.WithSpanKind(trace.SpanKindClient) }

// Internal returns SpanKind = INTERNAL (internal operation).
func Internal() trace.SpanStartOption { return trace.WithSpanKind(trace.SpanKindInternal) }

// ── Propagation ────────────────────────────────────────────────────────────

// InjectHeaders injects the current trace context into the provided headers.
// Use when making an outgoing HTTP call to another service.
func InjectHeaders(ctx context.Context, headers map[string]string) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(headers))
}

// ExtractHeaders extracts the trace context from the headers and returns an enriched context.
// Use when receiving an incoming HTTP call from another service.
func ExtractHeaders(ctx context.Context, headers map[string]string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(headers))
}

// ── Logging ────────────────────────────────────────────────────────────────

// Log emits a structured log at the given severity level.
func Log(ctx context.Context, level Level, msg string, fields ...F) {
	merged := mergeF(fields...)
	sev, sevText := levelToOtel(level)

	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetBody(otellog.StringValue(msg))
	rec.SetSeverity(sev)
	rec.SetSeverityText(sevText)
	for k, v := range merged {
		rec.AddAttributes(toLogAttr(k, v))
	}

	global.Logger(svcName()).Emit(ctx, rec)
}

// Info logs at INFO level.
func Info(ctx context.Context, msg string, fields ...F) { Log(ctx, INFO, msg, fields...) }

// Warn logs at WARN level.
func Warn(ctx context.Context, msg string, fields ...F) { Log(ctx, WARN, msg, fields...) }

// Debug logs at DEBUG level.
func Debug(ctx context.Context, msg string, fields ...F) { Log(ctx, DEBUG, msg, fields...) }

// Error logs at ERROR level. If err is non-nil, adds the "error" field.
func Error(ctx context.Context, msg string, err error, fields ...F) {
	if err != nil {
		fields = append(fields, F{"error": err.Error()})
	}
	Log(ctx, ERROR, msg, fields...)
}

// Fatal logs at FATAL level.
func Fatal(ctx context.Context, msg string, err error, fields ...F) {
	if err != nil {
		fields = append(fields, F{"error": err.Error()})
	}
	Log(ctx, FATAL, msg, fields...)
}

// ── Metrics ────────────────────────────────────────────────────────────────

// Count increments an Int64Counter by n.
//
//	telm.Count(ctx, "http.requests", 1, telm.F{"method": "GET", "status": 200})
func Count(ctx context.Context, name string, n int64, fields ...F) {
	getCounter(name).Add(ctx, n, metric.WithAttributes(toAttrs(mergeF(fields...))...))
}

// Record records a float64 value in a histogram.
//
//	telm.Record(ctx, "http.duration_ms", 42.5, telm.F{"route": "/api/users"})
func Record(ctx context.Context, name string, value float64, fields ...F) {
	getHistogram(name).Record(ctx, value, metric.WithAttributes(toAttrs(mergeF(fields...))...))
}

// Gauge adjusts an Int64UpDownCounter (values that can go up and down).
//
//	telm.Gauge(ctx, "connections.active", +1, telm.F{})
//	telm.Gauge(ctx, "connections.active", -1, telm.F{})
func Gauge(ctx context.Context, name string, delta int64, fields ...F) {
	getGauge(name).Add(ctx, delta, metric.WithAttributes(toAttrs(mergeF(fields...))...))
}

// ── Internal ───────────────────────────────────────────────────────────────

func mergeF(fields ...F) F {
	out := F{}
	for _, f := range fields {
		for k, v := range f {
			out[k] = v
		}
	}
	return out
}

func levelToOtel(l Level) (otellog.Severity, string) {
	switch l {
	case DEBUG:
		return otellog.SeverityDebug, "DEBUG"
	case INFO:
		return otellog.SeverityInfo, "INFO"
	case WARN:
		return otellog.SeverityWarn, "WARN"
	case ERROR:
		return otellog.SeverityError, "ERROR"
	case FATAL:
		return otellog.SeverityFatal, "FATAL"
	default:
		return otellog.SeverityInfo, "INFO"
	}
}

func toLogAttr(k string, v any) otellog.KeyValue {
	switch val := v.(type) {
	case string:
		return otellog.String(k, val)
	case int:
		return otellog.Int(k, val)
	case int64:
		return otellog.Int64(k, val)
	case float64:
		return otellog.Float64(k, val)
	case bool:
		return otellog.Bool(k, val)
	default:
		return otellog.String(k, fmt.Sprintf("%v", val))
	}
}

func toAttrs(f F) []attribute.KeyValue {
	kvs := make([]attribute.KeyValue, 0, len(f))
	for k, v := range f {
		switch val := v.(type) {
		case string:
			kvs = append(kvs, attribute.String(k, val))
		case int:
			kvs = append(kvs, attribute.Int(k, val))
		case int64:
			kvs = append(kvs, attribute.Int64(k, val))
		case float64:
			kvs = append(kvs, attribute.Float64(k, val))
		case bool:
			kvs = append(kvs, attribute.Bool(k, val))
		default:
			kvs = append(kvs, attribute.String(k, fmt.Sprintf("%v", val)))
		}
	}
	return kvs
}

func getCounter(name string) metric.Int64Counter {
	if v, ok := counters.Load(name); ok {
		return v.(metric.Int64Counter)
	}
	c, _ := otel.Meter(svcName()).Int64Counter(name)
	actual, _ := counters.LoadOrStore(name, c)
	return actual.(metric.Int64Counter)
}

func getHistogram(name string) metric.Float64Histogram {
	if v, ok := histograms.Load(name); ok {
		return v.(metric.Float64Histogram)
	}
	h, _ := otel.Meter(svcName()).Float64Histogram(name)
	actual, _ := histograms.LoadOrStore(name, h)
	return actual.(metric.Float64Histogram)
}

func getGauge(name string) metric.Int64UpDownCounter {
	if v, ok := gauges.Load(name); ok {
		return v.(metric.Int64UpDownCounter)
	}
	g, _ := otel.Meter(svcName()).Int64UpDownCounter(name)
	actual, _ := gauges.LoadOrStore(name, g)
	return actual.(metric.Int64UpDownCounter)
}
