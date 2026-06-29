// Package middleware provides structured logging for HTTP requests.
// The logging middleware captures request details, response status, duration,
// and contextual fields for distributed tracing and debugging.
package middleware

import (
	"net/http"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// LoggingMiddleware implements structured HTTP request logging.
type LoggingMiddleware struct {
	logger *zap.Logger
}

// NewLoggingMiddleware creates a new logging middleware.
func NewLoggingMiddleware(logger *zap.Logger) *LoggingMiddleware {
	return &LoggingMiddleware{
		logger: logger.Named("http"),
	}
}

// Handler returns the HTTP handler that logs requests.
func (lm *LoggingMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a response writer that captures status code and response size
		capture := &loggingResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		// Extract request details
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = "unknown"
		}

		clientIP := lm.getClientIP(r)
		userAgent := r.UserAgent()
		referer := r.Referer()

		// Execute the request
		next.ServeHTTP(capture, r)

		// Calculate duration
		duration := time.Since(start)

		// Build log fields
		fields := []zapcore.Field{
			zap.String("request_id", requestID),
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("query", r.URL.RawQuery),
			zap.Int("status_code", capture.statusCode),
			zap.Int64("response_size", capture.responseSize),
			zap.Duration("duration", duration),
			zap.Duration("duration_ms", duration.Round(time.Microsecond)),
			zap.String("client_ip", clientIP),
			zap.String("user_agent", userAgent),
			zap.String("referer", referer),
			zap.String("proto", r.Proto),
			zap.String("host", r.Host),
		}

		// Add user info if authenticated
		if userID := GetUserID(r.Context()); userID != "" {
			fields = append(fields, zap.String("user_id", userID))
		}

		// Log based on status code
		switch {
		case capture.statusCode >= 500:
			lm.logger.Error("server error",
				append(fields, zap.String("severity", "ERROR"))...,
			)
		case capture.statusCode >= 400:
			lm.logger.Warn("client error",
				append(fields, zap.String("severity", "WARNING"))...,
			)
		case duration > 500*time.Millisecond:
			lm.logger.Warn("slow request",
				append(fields, zap.String("severity", "WARNING"))...,
			)
		default:
			lm.logger.Info("request completed",
				append(fields, zap.String("severity", "INFO"))...,
			)
		}
	})
}

// getClientIP extracts the real client IP from the request.
func (lm *LoggingMiddleware) getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header (common in proxied environments)
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		// Return the first IP in the chain (closest to the client)
		if idx := len(xff); idx > 0 {
			for i := 0; i < len(xff); i++ {
				if xff[i] == ',' {
					return xff[:i]
				}
			}
			return xff
		}
	}

	// Check X-Real-IP header
	xri := r.Header.Get("X-Real-IP")
	if xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	return r.RemoteAddr
}

// loggingResponseWriter wraps http.ResponseWriter to capture status code and response size.
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode    int
	responseSize  int64
	headerWritten bool
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	if !lrw.headerWritten {
		lrw.statusCode = code
		lrw.headerWritten = true
		lrw.ResponseWriter.WriteHeader(code)
	}
}

func (lrw *loggingResponseWriter) Write(b []byte) (int, error) {
	if !lrw.headerWritten {
		lrw.WriteHeader(http.StatusOK)
	}
	n, err := lrw.ResponseWriter.Write(b)
	lrw.responseSize += int64(n)
	return n, err
}

// Flush implements http.Flusher.
func (lrw *loggingResponseWriter) Flush() {
	if f, ok := lrw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
