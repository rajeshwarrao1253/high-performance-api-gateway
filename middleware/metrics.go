// Package middleware provides Prometheus metrics collection for the API gateway.
// It tracks request counts, latency histograms, active connections, and errors by status code.
package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

var (
	// RequestCount tracks the total number of HTTP requests.
	RequestCount = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total number of HTTP requests processed by the gateway",
		},
		[]string{"method", "path", "status_code", "upstream"},
	)

	// RequestDuration tracks the latency of HTTP requests.
	RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "HTTP request latency in seconds",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~16s
		},
		[]string{"method", "path", "upstream"},
	)

	// ActiveConnections tracks the number of currently active connections.
	ActiveConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "gateway_active_connections",
			Help: "Number of currently active HTTP connections",
		},
	)

	// RequestSize tracks the size of HTTP requests.
	RequestSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_request_size_bytes",
			Help:    "HTTP request size in bytes",
			Buckets: prometheus.ExponentialBuckets(64, 4, 10),
		},
		[]string{"method", "path"},
	)

	// ResponseSize tracks the size of HTTP responses.
	ResponseSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_response_size_bytes",
			Help:    "HTTP response size in bytes",
			Buckets: prometheus.ExponentialBuckets(256, 4, 10),
		},
		[]string{"method", "path", "status_code"},
	)

	// ErrorsByStatus tracks errors grouped by status code.
	ErrorsByStatus = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_errors_total",
			Help: "Total number of HTTP error responses",
		},
		[]string{"status_code", "path", "upstream"},
	)

	// UpstreamHealth tracks the health status of upstream targets.
	UpstreamHealth = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gateway_upstream_health",
			Help: "Health status of upstream targets (1 = healthy, 0 = unhealthy)",
		},
		[]string{"upstream", "target"},
	)

	// CacheHits tracks cache hit/miss rates.
	CacheHits = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_cache_hits_total",
			Help: "Total number of cache hits and misses",
		},
		[]string{"result", "path"},
	)

	// RateLimitedRequests tracks requests that were rate limited.
	RateLimitedRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_rate_limited_total",
			Help: "Total number of rate limited requests",
		},
		[]string{"path", "client_id"},
	)

	// MiddlewareDuration tracks the time spent in each middleware.
	MiddlewareDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_middleware_duration_seconds",
			Help:    "Time spent in middleware execution",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12),
		},
		[]string{"middleware", "path"},
	)

	// CircuitBreakerState tracks the current state of circuit breakers.
	CircuitBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gateway_circuit_breaker_state",
			Help: "Current state of circuit breakers (0=closed, 1=half-open, 2=open)",
		},
		[]string{"endpoint"},
	)
)

// MetricsMiddleware collects Prometheus metrics for HTTP requests.
type MetricsMiddleware struct {
	logger *zap.Logger
}

// NewMetricsMiddleware creates a new metrics collection middleware.
func NewMetricsMiddleware(logger *zap.Logger) *MetricsMiddleware {
	return &MetricsMiddleware{
		logger: logger.Named("metrics"),
	}
}

// Handler returns the HTTP handler that collects metrics.
func (mm *MetricsMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Track active connection
		ActiveConnections.Inc()
		defer ActiveConnections.Dec()

		// Record request size if Content-Length is available
		if r.ContentLength > 0 {
			RequestSize.WithLabelValues(r.Method, r.URL.Path).Observe(float64(r.ContentLength))
		}

		// Capture response details
		capture := &metricsResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		// Execute request
		next.ServeHTTP(capture, r)

		// Calculate duration
		duration := time.Since(start).Seconds()

		// Get upstream name from context if available
		upstream := r.URL.Host
		if upstream == "" {
			upstream = "unknown"
		}

		// Record metrics
		statusCode := strconv.Itoa(capture.statusCode)

		RequestCount.WithLabelValues(r.Method, r.URL.Path, statusCode, upstream).Inc()
		RequestDuration.WithLabelValues(r.Method, r.URL.Path, upstream).Observe(duration)
		ResponseSize.WithLabelValues(r.Method, r.URL.Path, statusCode).Observe(float64(capture.responseSize))

		// Track errors (4xx and 5xx)
		if capture.statusCode >= 400 {
			ErrorsByStatus.WithLabelValues(statusCode, r.URL.Path, upstream).Inc()
		}
	})
}

// metricsResponseWriter wraps http.ResponseWriter to capture response metrics.
type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode   int
	responseSize int64
	written      bool
}

func (mrw *metricsResponseWriter) WriteHeader(code int) {
	if !mrw.written {
		mrw.statusCode = code
		mrw.written = true
		mrw.ResponseWriter.WriteHeader(code)
	}
}

func (mrw *metricsResponseWriter) Write(b []byte) (int, error) {
	if !mrw.written {
		mrw.WriteHeader(http.StatusOK)
	}
	n, err := mrw.ResponseWriter.Write(b)
	mrw.responseSize += int64(n)
	return n, err
}

// Flush implements http.Flusher.
func (mrw *metricsResponseWriter) Flush() {
	if f, ok := mrw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// RegisterUpstreamHealth registers the health status of an upstream target.
func RegisterUpstreamHealth(upstream, target string, healthy bool) {
	value := 0.0
	if healthy {
		value = 1.0
	}
	UpstreamHealth.WithLabelValues(upstream, target).Set(value)
}

// RecordCacheHit records a cache hit metric.
func RecordCacheHit(path string) {
	CacheHits.WithLabelValues("hit", path).Inc()
}

// RecordCacheMiss records a cache miss metric.
func RecordCacheMiss(path string) {
	CacheHits.WithLabelValues("miss", path).Inc()
}

// RecordRateLimited records a rate limit event.
func RecordRateLimited(path, clientID string) {
	RateLimitedRequests.WithLabelValues(path, clientID).Inc()
}

// RecordCircuitBreakerState records the circuit breaker state.
func RecordCircuitBreakerState(endpoint string, state int) {
	CircuitBreakerState.WithLabelValues(endpoint).Set(float64(state))
}

// RecordMiddlewareDuration records the time spent in a middleware.
func RecordMiddlewareDuration(middlewareName, path string, duration time.Duration) {
	MiddlewareDuration.WithLabelValues(middlewareName, path).Observe(duration.Seconds())
}
