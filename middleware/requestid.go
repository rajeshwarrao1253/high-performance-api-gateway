// Package middleware provides request ID generation and propagation.
// Each incoming request is assigned a unique request ID that is propagated
// through the request lifecycle and included in response headers for tracing.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// HeaderRequestID is the HTTP header used for request ID propagation.
	HeaderRequestID = "X-Request-ID"
	// HeaderTraceID is the HTTP header used for trace ID propagation.
	HeaderTraceID = "X-Trace-ID"
)

// RequestIDKey is the context key for storing the request ID.
type RequestIDKey struct{}

// GenerateRequestID generates a new unique request ID.
// It uses UUID v4 by default, falling back to a random hex string.
func GenerateRequestID() string {
	// Try UUID first
	id, err := uuid.NewRandom()
	if err == nil {
		return id.String()
	}

	// Fallback to random bytes
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Last resort: timestamp-based ID
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

// RequestIDMiddleware creates a middleware that ensures every request has a unique ID.
// It checks for an existing request ID from upstream (e.g., load balancer) and
// generates a new one if not present. The ID is added to the response headers
// and stored in the request context for downstream use.
func RequestIDMiddleware(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check for existing request ID from upstream
			requestID := r.Header.Get(HeaderRequestID)
			if requestID == "" {
				// Check alternative header
				requestID = r.Header.Get(HeaderTraceID)
			}

			// Generate new request ID if none provided
			if requestID == "" {
				requestID = GenerateRequestID()
			}

			// Add request ID to response headers
			w.Header().Set(HeaderRequestID, requestID)

			// Store in context for downstream middleware and handlers
			ctx := context.WithValue(r.Context(), RequestIDKey{}, requestID)
			r = r.WithContext(ctx)

			// Log with request ID
			logger.Debug("request id assigned",
				zap.String("request_id", requestID),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
			)

			next.ServeHTTP(w, r)
		})
	}
}

// GetRequestID extracts the request ID from the context.
// Returns an empty string if no request ID is present.
func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value(RequestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// SetRequestID adds a request ID to the context.
func SetRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, RequestIDKey{}, requestID)
}

// RequestIDInjector returns a middleware that injects request IDs into outgoing requests.
// This is useful for service-to-service communication where the request ID
// should be propagated to upstream services.
func RequestIDInjector() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := GetRequestID(r.Context())
			if requestID != "" {
				r.Header.Set(HeaderRequestID, requestID)
			}
			next.ServeHTTP(w, r)
		})
	}
}
