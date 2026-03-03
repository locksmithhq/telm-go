# telm-go SDK

Go SDK for [Telm](https://github.com/locksmithhq/telm) — a minimal, ergonomic observability library that instruments your application with **traces**, **structured logs**, and **metrics** using OpenTelemetry under the hood.

All telemetry is exported via OTLP/HTTP directly to the Telm collector.

---

## Features

- **Distributed tracing** — spans with automatic error recording and W3C TraceContext propagation
- **Structured logging** — severity-leveled logs correlated to the active span
- **Metrics** — counters, histograms and gauges with typed attributes
- **HTTP middleware** — automatic instrumentation for any `net/http`-compatible router
- **Runtime metrics** — CPU, memory, goroutines, GC pause and I/O collected automatically

---

## Installation

```bash
go get github.com/locksmithhq/telm-go
```

Requires Go 1.22+.

---

## Quick start

```go
package main

import (
    "context"
    "log"

    "github.com/locksmithhq/telm-go"
)

func main() {
    ctx := context.Background()

    shutdown, err := telm.Init(ctx,
        telm.WithServiceName("my-service"),
        telm.WithEndpoint("telm-collector:4318"),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer shutdown(ctx)

    ctx, end := telm.Start(ctx, "main.operation")
    defer end(nil)

    telm.Info(ctx, "service started", telm.F{"env": "production"})
}
```

---

## Initialization

`telm.Init` sets up the OTel providers (trace, metric, log) and starts collecting runtime metrics. Call it once at application startup and defer the returned shutdown function.

```go
shutdown, err := telm.Init(ctx,
    telm.WithServiceName("order-service"),  // identifies the service in Telm
    telm.WithEndpoint("telm-collector:4318"), // Telm collector address (no scheme)
)
if err != nil {
    log.Fatal(err)
}
defer shutdown(ctx)
```

| Option | Default | Description |
|---|---|---|
| `WithServiceName(name)` | `"unknown"` | Service name attached to all telemetry |
| `WithEndpoint(addr)` | `"localhost:4318"` | Telm collector address (`host:port`) |

---

## Tracing

### Opening a span

```go
ctx, end := telm.Start(ctx, "db.query")
defer end(nil)

// on error, pass it to end — it will be recorded on the span automatically
result, err := db.QueryContext(ctx, query)
if err != nil {
    end(err)
    return err
}
end(nil)
```

### Span kinds

```go
// Incoming request (e.g. HTTP handler, gRPC server)
ctx, end := telm.Start(ctx, "POST /orders", telm.Server())
defer end(nil)

// Outgoing call (e.g. HTTP client, database, message broker)
ctx, end := telm.Start(ctx, "redis.get", telm.Client())
defer end(nil)

// Internal logic
ctx, end := telm.Start(ctx, "order.validate", telm.Internal())
defer end(nil)
```

### Adding attributes to a span

```go
ctx, end := telm.Start(ctx, "process.order")
defer end(nil)

telm.Attr(ctx, telm.F{
    "order.id":     orderID,
    "order.amount": 149.90,
    "customer.id":  customerID,
})
```

### Recording events

```go
telm.Event(ctx, "cache.hit", telm.F{"key": cacheKey})
telm.Event(ctx, "retry.attempt", telm.F{"attempt": 2, "reason": "timeout"})
```

### Propagating trace context across services

**Outgoing call (inject):**

```go
// With *http.Request
req, _ := http.NewRequestWithContext(ctx, "POST", "http://payment-svc/charge", body)
telm.InjectHTTPRequest(ctx, req)
resp, err := http.DefaultClient.Do(req)

// With a generic map (e.g. message queue headers, gRPC metadata)
headers := map[string]string{}
telm.InjectHeaders(ctx, headers)
```

**Incoming call (extract):**

```go
// With *http.Request — handled automatically by HTTPMiddleware
// For other transports, extract manually:
headers := map[string]string{
    "traceparent": msg.Header.Get("traceparent"),
}
ctx = telm.ExtractHeaders(ctx, headers)
ctx, end := telm.Start(ctx, "consume.order.created")
defer end(nil)
```

---

## Logging

All log functions accept optional `telm.F` maps and are automatically correlated to the active span in the context.

```go
telm.Debug(ctx, "cache lookup", telm.F{"key": "user:42"})
telm.Info(ctx, "order created", telm.F{"order_id": "ord_123", "amount": 99.90})
telm.Warn(ctx, "rate limit approaching", telm.F{"used": 980, "limit": 1000})
telm.Error(ctx, "payment failed", err, telm.F{"order_id": "ord_123"})
telm.Fatal(ctx, "database unreachable", err)
```

For dynamic log levels:

```go
telm.Log(ctx, telm.WARN, "unusual latency", telm.F{"p99_ms": 850})
```

| Function | Level |
|---|---|
| `Debug` | DEBUG |
| `Info` | INFO |
| `Warn` | WARN |
| `Error` | ERROR |
| `Fatal` | FATAL |

---

## Metrics

Metric names must be **static strings**. Dynamic values belong in the `telm.F` attributes, not in the name.

### Counter

Monotonically increasing. Use for totals: requests, errors, events.

```go
telm.Count(ctx, "orders.created", 1, telm.F{
    "plan":   "pro",
    "region": "us-east-1",
})
```

### Histogram

Distribution of values. Use for latencies, sizes, durations.

```go
start := time.Now()
// ... operation ...
telm.Record(ctx, "db.query.duration_ms", float64(time.Since(start).Milliseconds()), telm.F{
    "table":     "orders",
    "operation": "select",
})
```

### Gauge

Values that can go up and down. Use for connection pools, queue depths, active jobs.

```go
// increment
telm.Gauge(ctx, "workers.active", +1, telm.F{"pool": "default"})

// decrement
defer telm.Gauge(ctx, "workers.active", -1, telm.F{"pool": "default"})
```

---

## HTTP Middleware

`HTTPMiddleware` automatically instruments every request with a trace, metrics, and a log entry. It is compatible with any router that accepts `func(http.Handler) http.Handler`.

### net/http

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /api/orders/{id}", getOrderHandler)
mux.HandleFunc("POST /api/orders", createOrderHandler)

http.ListenAndServe(":8080", telm.HTTPMiddleware()(mux))
```

### chi

```go
r := chi.NewRouter()
r.Use(telm.HTTPMiddleware(
    telm.WithRouteResolver(func(r *http.Request) string {
        if rc := chi.RouteContext(r.Context()); rc != nil {
            if p := rc.RoutePattern(); p != "" {
                return p
            }
        }
        return r.URL.Path
    }),
    telm.WithSlowThreshold(500*time.Millisecond),
    telm.WithSkipPaths("/health", "/ready", "/metrics"),
))
```

### gorilla/mux

```go
r := mux.NewRouter()
r.Use(telm.HTTPMiddleware(
    telm.WithRouteResolver(func(r *http.Request) string {
        if route := mux.CurrentRoute(r); route != nil {
            if tpl, err := route.GetPathTemplate(); err == nil {
                return tpl
            }
        }
        return r.URL.Path
    }),
))
```

### Middleware options

| Option | Default | Description |
|---|---|---|
| `WithRouteResolver(fn)` | `r.Pattern` or `r.URL.Path` | Extracts the route pattern from the request |
| `WithSlowThreshold(d)` | `1s` | Requests slower than this are logged as WARN |
| `WithSkipPaths(paths...)` | none | Paths that bypass instrumentation entirely |

### What gets recorded per request

**Trace** — SERVER span with attributes:

```
http.request.method        GET, POST, …
http.route                 /api/orders/{id}
http.response.status_code  200, 404, 500, …
http.request.body.size     bytes received
http.response.body.size    bytes sent
net.peer.addr              client IP (respects X-Forwarded-For)
user_agent.original        User-Agent header
server.address             Host header
```

**Metrics:**

```
http.server.requests.total       counter    total requests
http.server.request.duration_ms  histogram  latency in ms
http.server.request.body_bytes   histogram  request body size
http.server.response.body_bytes  histogram  response body size
http.server.requests.active      gauge      in-flight requests
http.server.errors.total         counter    4xx and 5xx responses
```

All metrics carry `method`, `route`, `status_code` and `status_class` attributes.

**Log** — one structured log per request:

```
INFO   2xx / 3xx responses
WARN   4xx responses or duration above slowThreshold
ERROR  5xx responses (with error field)
```

---

## Runtime metrics

Collected automatically after `telm.Init` — no extra code required.

| Metric | Unit | Description |
|---|---|---|
| `process.cpu.usage` | `%` | CPU usage in the current collection interval |
| `process.memory.bytes` | `By` | Heap memory allocated |
| `runtime.goroutines` | — | Number of active goroutines |
| `runtime.gc.pause_ms` | `ms` | Last GC pause duration |
| `process.io.read_bytes` | `By` | Bytes read in the last interval |
| `process.io.write_bytes` | `By` | Bytes written in the last interval |

> Runtime metrics rely on `/proc/self/stat` and `/proc/self/io` and are only available on Linux.

---

## Complete example

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "time"

    "github.com/locksmithhq/telm-go"
)

func main() {
    ctx := context.Background()

    shutdown, err := telm.Init(ctx,
        telm.WithServiceName("checkout-service"),
        telm.WithEndpoint("telm-collector:4318"),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer shutdown(ctx)

    mux := http.NewServeMux()
    mux.HandleFunc("POST /checkout", checkoutHandler)
    mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
    })

    handler := telm.HTTPMiddleware(
        telm.WithSlowThreshold(800*time.Millisecond),
        telm.WithSkipPaths("/health"),
    )(mux)

    log.Println("listening on :8080")
    log.Fatal(http.ListenAndServe(":8080", handler))
}

func checkoutHandler(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()

    order, err := processOrder(ctx, r)
    if err != nil {
        telm.Error(ctx, "checkout failed", err, telm.F{"path": r.URL.Path})
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }

    telm.Info(ctx, "checkout complete", telm.F{"order_id": order.ID})
    fmt.Fprintf(w, `{"order_id": "%s"}`, order.ID)
}

type Order struct{ ID string }

func processOrder(ctx context.Context, r *http.Request) (*Order, error) {
    ctx, end := telm.Start(ctx, "order.process", telm.Internal())
    defer end(nil)

    order, err := saveOrder(ctx)
    if err != nil {
        end(err)
        return nil, err
    }

    if err := chargePayment(ctx, order); err != nil {
        end(err)
        return nil, err
    }

    telm.Count(ctx, "orders.created", 1, telm.F{"plan": "pro"})
    return order, nil
}

func saveOrder(ctx context.Context) (*Order, error) {
    ctx, end := telm.Start(ctx, "db.orders.insert", telm.Client())
    defer end(nil)

    start := time.Now()
    // ... database call ...
    telm.Record(ctx, "db.query.duration_ms",
        float64(time.Since(start).Milliseconds()),
        telm.F{"table": "orders", "op": "insert"},
    )

    return &Order{ID: "ord_abc123"}, nil
}

func chargePayment(ctx context.Context, order *Order) error {
    ctx, end := telm.Start(ctx, "payment.charge", telm.Client())
    defer end(nil)

    // propagate trace context to the payment service
    req, _ := http.NewRequestWithContext(ctx, "POST", "http://payment-svc/charge", nil)
    telm.InjectHTTPRequest(ctx, req)

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    telm.Attr(ctx, telm.F{
        "order.id":          order.ID,
        "payment.status":    resp.StatusCode,
    })
    return nil
}
```
