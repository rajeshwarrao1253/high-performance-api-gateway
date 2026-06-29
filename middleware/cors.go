// Package middleware provides Cross-Origin Resource Sharing (CORS) support.
// The CORS middleware handles preflight requests and adds appropriate headers
// based on configurable origins, methods, and headers.
package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
)

// CORSMiddleware handles Cross-Origin Resource Sharing for HTTP requests.
type CORSMiddleware struct {
	config config.CORSConfig
	logger *zap.Logger

	// Pre-computed lookup sets for efficient matching
	allowedOrigins map[string]bool
	allowedMethods map[string]bool
	allowedHeaders map[string]bool
}

// NewCORSMiddleware creates a new CORS middleware.
func NewCORSMiddleware(cfg config.CORSConfig, logger *zap.Logger) *CORSMiddleware {
	cm := &CORSMiddleware{
		config:         cfg,
		logger:         logger.Named("cors"),
		allowedOrigins: make(map[string]bool, len(cfg.AllowedOrigins)),
		allowedMethods: make(map[string]bool, len(cfg.AllowedMethods)),
		allowedHeaders: make(map[string]bool, len(cfg.AllowedHeaders)),
	}

	// Build lookup sets for O(1) matching
	for _, origin := range cfg.AllowedOrigins {
		cm.allowedOrigins[origin] = true
	}
	for _, method := range cfg.AllowedMethods {
		cm.allowedMethods[method] = true
	}
	for _, header := range cfg.AllowedHeaders {
		cm.allowedHeaders[strings.ToLower(header)] = true
	}

	return cm
}

// Handler returns the HTTP handler that adds CORS headers.
func (cm *CORSMiddleware) Handler(next http.Handler) http.Handler {
	if !cm.config.Enabled {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Check if origin is allowed
		if !cm.isOriginAllowed(origin) {
			// Origin not allowed, but still process the request (no CORS headers)
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")

		if cm.config.AllowCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		// Set exposed headers
		if len(cm.config.ExposedHeaders) > 0 {
			w.Header().Set("Access-Control-Expose-Headers", strings.Join(cm.config.ExposedHeaders, ", "))
		}

		// Handle preflight (OPTIONS) requests
		if r.Method == http.MethodOptions {
			cm.handlePreflight(w, r)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handlePreflight handles CORS preflight (OPTIONS) requests.
func (cm *CORSMiddleware) handlePreflight(w http.ResponseWriter, r *http.Request) {
	// Handle Access-Control-Request-Method
	if requestedMethod := r.Header.Get("Access-Control-Request-Method"); requestedMethod != "" {
		if cm.allowedMethods[requestedMethod] || cm.allowedMethods["*"] {
			w.Header().Set("Access-Control-Allow-Methods", strings.Join(cm.config.AllowedMethods, ", "))
		}
	}

	// Handle Access-Control-Request-Headers
	if requestedHeaders := r.Header.Get("Access-Control-Request-Headers"); requestedHeaders != "" {
		headers := strings.Split(requestedHeaders, ",")
		var allowed []string
		for _, header := range headers {
			header = strings.TrimSpace(strings.ToLower(header))
			if cm.allowedHeaders[header] || cm.allowedHeaders["*"] {
				allowed = append(allowed, header)
			}
		}
		if len(allowed) > 0 {
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(allowed, ", "))
		}
	}

	// Set max age
	if cm.config.MaxAge > 0 {
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(cm.config.MaxAge))
	}
}

// isOriginAllowed checks if the given origin is allowed.
func (cm *CORSMiddleware) isOriginAllowed(origin string) bool {
	// Allow all origins if wildcard is configured
	if cm.allowedOrigins["*"] {
		return true
	}

	// Check exact match
	if cm.allowedOrigins[origin] {
		return true
	}

	// Check for wildcard domain matching (e.g., *.example.com)
	for allowed := range cm.allowedOrigins {
		if strings.HasPrefix(allowed, "*.") {
			domain := allowed[1:] // Remove the leading *
			if strings.HasSuffix(origin, domain) {
				return true
			}
		}
	}

	return false
}

// DefaultCORSConfig returns a default CORS configuration that allows common settings.
func DefaultCORSConfig() config.CORSConfig {
	return config.CORSConfig{
		Enabled:          true,
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Request-ID", "X-API-Key"},
		ExposedHeaders:   []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-Request-ID"},
		AllowCredentials: false,
		MaxAge:           86400,
	}
}

// StrictCORSConfig returns a strict CORS configuration for production use.
func StrictCORSConfig(origins []string) config.CORSConfig {
	return config.CORSConfig{
		Enabled:          true,
		AllowedOrigins:   origins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Request-ID"},
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           3600,
	}
}
