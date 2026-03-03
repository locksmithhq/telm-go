package telm

// http.go — automatic HTTP instrumentation middleware.
//
// Compatible with any router that accepts func(http.Handler) http.Handler:
// net/http, chi, gorilla/mux, echo (with adapter), fiber (with adapter), etc.
//
// What is instrumented per request:
//
//	Traces  → SERVER span with HTTP semconv attributes, W3C TraceContext propagation
//	Metrics → requests.total, duration, body sizes, active, errors
//	Logs    → structured log correlated to the span (INFO / WARN / ERROR)

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// ── Options ───────────────────────────────────────────────────────────────────

type httpConfig struct {
	routeResolver func(*http.Request) string
	slowThreshold time.Duration
	skipPaths     map[string]struct{}
}

// HTTPOption configures the HTTPMiddleware.
type HTTPOption func(*httpConfig)

// WithRouteResolver sets a custom function to extract the route pattern.
//
// Defaults to r.Pattern (net/http Go 1.22+) or r.URL.Path as fallback.
// For other routers:
//
//	// chi
//	telm.WithRouteResolver(func(r *http.Request) string {
//	    if rc := chi.RouteContext(r.Context()); rc != nil {
//	        if p := rc.RoutePattern(); p != "" {
//	            return p
//	        }
//	    }
//	    return r.URL.Path
//	})
//
//	// gorilla/mux
//	telm.WithRouteResolver(func(r *http.Request) string {
//	    if route := mux.CurrentRoute(r); route != nil {
//	        if tpl, err := route.GetPathTemplate(); err == nil {
//	            return tpl
//	        }
//	    }
//	    return r.URL.Path
//	})
func WithRouteResolver(fn func(*http.Request) string) HTTPOption {
	return func(c *httpConfig) { c.routeResolver = fn }
}

// WithSlowThreshold sets the duration threshold above which requests are logged as WARN.
// Defaults to 1s.
func WithSlowThreshold(d time.Duration) HTTPOption {
	return func(c *httpConfig) { c.slowThreshold = d }
}

// WithSkipPaths defines paths that will not be instrumented.
// Useful for /health, /ready, /metrics, /favicon.ico, etc.
//
//	telm.WithSkipPaths("/health", "/ready", "/metrics")
func WithSkipPaths(paths ...string) HTTPOption {
	return func(c *httpConfig) {
		for _, p := range paths {
			c.skipPaths[p] = struct{}{}
		}
	}
}

// ── Middleware ────────────────────────────────────────────────────────────────

// HTTPMiddleware returns a net/http middleware that instruments handlers with
// automatic traces, metrics, and logs.
//
// # Traces
//
// Extracts W3C TraceContext and Baggage from incoming HTTP headers. Opens a
// SERVER span wrapping the entire handler execution. Recorded attributes:
//
//	http.request.method        GET, POST, …
//	http.route                 /api/users/:id  (route pattern, not the real URL)
//	http.response.status_code  200, 404, 500, …
//	http.request.body.size     received bytes (when Content-Length > 0)
//	http.response.body.size    bytes sent
//	net.peer.addr              client IP (respects X-Forwarded-For)
//	user_agent.original        User-Agent header value
//	server.address             request Host
//
// # Metrics
//
//	http.server.requests.total         counter   method, route, status_code, status_class
//	http.server.request.duration_ms    histogram method, route, status_code, status_class
//	http.server.request.body_bytes     histogram method, route  (only when body > 0)
//	http.server.response.body_bytes    histogram method, route, status_code, status_class
//	http.server.requests.active        gauge     method, route
//	http.server.errors.total           counter   method, route, status_code, status_class  (4xx/5xx)
//
// # Logs
//
//	INFO   → normal responses (2xx/3xx)
//	WARN   → 4xx or duration above slowThreshold
//	ERROR  → 5xx with the error object attached
//
// # Usage
//
//	// net/http
//	mux := http.NewServeMux()
//	mux.HandleFunc("GET /api/users/{id}", usersHandler)
//	http.ListenAndServe(":8080", telm.HTTPMiddleware()(mux))
//
//	// chi
//	r := chi.NewRouter()
//	r.Use(telm.HTTPMiddleware(
//	    telm.WithRouteResolver(chiRoutePattern),
//	    telm.WithSlowThreshold(500*time.Millisecond),
//	    telm.WithSkipPaths("/health", "/ready"),
//	))
//
//	// any router accepting func(http.Handler) http.Handler
//	router.Use(telm.HTTPMiddleware())
func HTTPMiddleware(opts ...HTTPOption) func(http.Handler) http.Handler {
	cfg := &httpConfig{
		slowThreshold: time.Second,
		skipPaths:     make(map[string]struct{}),
		routeResolver: defaultRouteResolver,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// ── skip ──────────────────────────────────────────────────────
			if _, skip := cfg.skipPaths[r.URL.Path]; skip {
				next.ServeHTTP(w, r)
				return
			}

			// ── trace context propagation ──────────────────────────────────
			// HeaderCarrier respects HTTP header canonicalization
			// (e.g. "Traceparent" → "traceparent" when looked up by the propagator)
			ctx := otel.GetTextMapPropagator().Extract(
				r.Context(),
				propagation.HeaderCarrier(r.Header),
			)

			// ── route + method ─────────────────────────────────────────────
			route := cfg.routeResolver(r)
			method := r.Method

			// ── active requests ────────────────────────────────────────────
			Gauge(ctx, "http.server.requests.active", +1, F{"method": method, "route": route})
			defer Gauge(ctx, "http.server.requests.active", -1, F{"method": method, "route": route})

			// ── SERVER span ────────────────────────────────────────────────
			ctx, end := Start(ctx, method+" "+route, Server())

			// ── response capture ───────────────────────────────────────────
			cw := &captureWriter{ResponseWriter: w, statusCode: http.StatusOK}

			start := time.Now()
			next.ServeHTTP(cw, r.WithContext(ctx))
			duration := time.Since(start)

			// ── captured values ────────────────────────────────────────────
			status := cw.statusCode
			reqBytes := requestBodySize(r)
			respBytes := cw.bytesWritten
			durationMs := float64(duration.Milliseconds())
			class := statusClass(status)
			peer := clientIP(r)

			// ── span attributes ────────────────────────────────────────────
			Attr(ctx, F{
				"http.request.method":       method,
				"http.route":                route,
				"http.response.status_code": status,
				"http.request.body.size":    reqBytes,
				"http.response.body.size":   respBytes,
				"net.peer.addr":             peer,
				"user_agent.original":       r.UserAgent(),
				"server.address":            r.Host,
			})

			// span ends with error on 5xx
			var spanErr error
			if status >= 500 {
				spanErr = fmt.Errorf("HTTP %d %s", status, http.StatusText(status))
			}
			end(spanErr)

			// ── metrics ────────────────────────────────────────────────────
			dims := F{
				"method":       method,
				"route":        route,
				"status_code":  status,
				"status_class": class,
			}

			Count(ctx, "http.server.requests.total", 1, dims)
			Record(ctx, "http.server.request.duration_ms", durationMs, dims)
			Record(ctx, "http.server.response.body_bytes", float64(respBytes), dims)

			if reqBytes > 0 {
				Record(ctx, "http.server.request.body_bytes", float64(reqBytes),
					F{"method": method, "route": route},
				)
			}
			if status >= 400 {
				Count(ctx, "http.server.errors.total", 1, dims)
			}

			// ── log ────────────────────────────────────────────────────────
			fields := F{
				"http.method":       method,
				"http.route":        route,
				"http.status_code":  status,
				"http.status_class": class,
				"http.duration_ms":  durationMs,
				"http.req_bytes":    reqBytes,
				"http.resp_bytes":   respBytes,
				"net.peer_addr":     peer,
				"user_agent":        r.UserAgent(),
				"server.host":       r.Host,
			}

			switch {
			case status >= 500:
				Error(ctx, "server error", spanErr, fields)
			case status >= 400:
				Warn(ctx, "client error", fields)
			case duration >= cfg.slowThreshold:
				fields["slow"] = true
				Warn(ctx, "slow request", fields)
			default:
				Info(ctx, "request", fields)
			}
		})
	}
}

// ── captureWriter ─────────────────────────────────────────────────────────────

// captureWriter intercepts the status code and counts bytes written in the response.
//
// Implements:
//   - http.Flusher  — SSE, chunked streaming
//   - http.Hijacker — WebSocket, HTTP/1.1 upgrade
//   - Unwrap()      — http.ResponseController chain (Go 1.20+)
type captureWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
	headerSent   bool
}

func (cw *captureWriter) WriteHeader(statusCode int) {
	if !cw.headerSent {
		cw.statusCode = statusCode
		cw.headerSent = true
		cw.ResponseWriter.WriteHeader(statusCode)
	}
}

func (cw *captureWriter) Write(b []byte) (int, error) {
	if !cw.headerSent {
		cw.WriteHeader(http.StatusOK)
	}
	n, err := cw.ResponseWriter.Write(b)
	cw.bytesWritten += int64(n)
	return n, err
}

// Flush implements http.Flusher. Delegates to the underlying writer if supported.
func (cw *captureWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker. Delegates to the underlying writer if supported.
func (cw *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := cw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("telm: ResponseWriter does not implement http.Hijacker")
}

// Unwrap returns the underlying ResponseWriter.
// Used by http.ResponseController (Go 1.20+) for interface promotion.
func (cw *captureWriter) Unwrap() http.ResponseWriter { return cw.ResponseWriter }

// ── helpers ───────────────────────────────────────────────────────────────────

// InjectHTTPRequest injects the current trace context into the headers of an outgoing
// HTTP request. Use when making outgoing HTTP calls to other services.
//
//	req, _ := http.NewRequestWithContext(ctx, "POST", url, body)
//	telm.InjectHTTPRequest(ctx, req)
//	resp, err := http.DefaultClient.Do(req)
func InjectHTTPRequest(ctx context.Context, req *http.Request) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// defaultRouteResolver uses r.Pattern (net/http Go 1.22+) or r.URL.Path.
func defaultRouteResolver(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.URL.Path
}

// requestBodySize returns Content-Length if positive, 0 otherwise.
func requestBodySize(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}

// clientIP returns the real client IP, respecting reverse proxy headers.
func clientIP(r *http.Request) string {
	// X-Forwarded-For may be "client, proxy1, proxy2"; takes the first entry
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if idx := strings.IndexByte(fwd, ','); idx != -1 {
			return strings.TrimSpace(fwd[:idx])
		}
		return strings.TrimSpace(fwd)
	}
	if real := r.Header.Get("X-Real-Ip"); real != "" {
		return strings.TrimSpace(real)
	}
	return r.RemoteAddr
}

// statusClass returns the HTTP status class as a string ("2xx", "4xx", etc).
func statusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
