// Package middleware provides a registry for managing and resolving middleware chains.
package middleware

import (
	"fmt"
	"net/http"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
)

// MiddlewareFunc is a function that wraps an HTTP handler.
type MiddlewareFunc func(http.Handler) http.Handler

// Registry manages middleware instances and resolves middleware chains.
type Registry struct {
	logger           *zap.Logger
	redis            *redis.Client
	config           *config.Config
	middlewares      map[string]MiddlewareFunc
	builtin          map[string]func() MiddlewareFunc
}

// NewRegistry creates a new middleware registry with all built-in middleware.
func NewRegistry(logger *zap.Logger, redisClient *redis.Client, cfg *config.Config) *Registry {
	r := &Registry{
		logger:      logger,
		redis:       redisClient,
		config:      cfg,
		middlewares: make(map[string]MiddlewareFunc),
		builtin:     make(map[string]func() MiddlewareFunc),
	}

	// Register built-in middleware factories
	r.builtin["auth"] = func() MiddlewareFunc {
		m := NewAuthMiddleware(cfg.JWT, logger)
		return m.Handler
	}

	r.builtin["rate_limit"] = func() MiddlewareFunc {
		m := NewRateLimitMiddleware(cfg.RateLimit, logger, redisClient)
		return m.Handler
	}

	r.builtin["circuit_breaker"] = func() MiddlewareFunc {
		m := NewCircuitBreaker(cfg.CircuitBreaker, logger)
		return m.Handler
	}

	r.builtin["cache"] = func() MiddlewareFunc {
		m := NewCacheMiddleware(cfg.Cache, logger, redisClient)
		return m.Handler
	}

	r.builtin["logging"] = func() MiddlewareFunc {
		m := NewLoggingMiddleware(logger)
		return m.Handler
	}

	r.builtin["metrics"] = func() MiddlewareFunc {
		m := NewMetricsMiddleware(logger)
		return m.Handler
	}

	r.builtin["cors"] = func() MiddlewareFunc {
		m := NewCORSMiddleware(cfg.CORS, logger)
		return m.Handler
	}

	r.builtin["request_id"] = func() MiddlewareFunc {
		return RequestIDMiddleware(logger)
	}

	return r
}

// Resolve converts a list of middleware names into an ordered chain of middleware functions.
// Unknown middleware names are logged and skipped.
func (r *Registry) Resolve(names []string) []MiddlewareFunc {
	var chain []MiddlewareFunc

	for _, name := range names {
		// Check if already instantiated
		if mw, ok := r.middlewares[name]; ok {
			chain = append(chain, mw)
			continue
		}

		// Check if it's a built-in middleware
		if factory, ok := r.builtin[name]; ok {
			mw := factory()
			r.middlewares[name] = mw
			chain = append(chain, mw)
			continue
		}

		// Unknown middleware - log warning
		r.logger.Warn("unknown middleware, skipping",
			zap.String("middleware", name),
		)
	}

	return chain
}

// Register adds a custom middleware to the registry.
func (r *Registry) Register(name string, middleware MiddlewareFunc) {
	r.middlewares[name] = middleware
	r.logger.Info("custom middleware registered",
		zap.String("name", name),
	)
}

// RegisterFactory adds a custom middleware factory to the registry.
func (r *Registry) RegisterFactory(name string, factory func() MiddlewareFunc) {
	r.builtin[name] = factory
	r.logger.Info("custom middleware factory registered",
		zap.String("name", name),
	)
}

// Get retrieves a middleware by name.
func (r *Registry) Get(name string) (MiddlewareFunc, bool) {
	// Check instantiated middlewares
	if mw, ok := r.middlewares[name]; ok {
		return mw, true
	}

	// Check builtin factories
	if factory, ok := r.builtin[name]; ok {
		mw := factory()
		r.middlewares[name] = mw
		return mw, true
	}

	return nil, false
}

// Chain combines multiple middleware functions into a single chain.
// The first middleware in the list is the outermost (executes first).
func Chain(middlewares ...MiddlewareFunc) MiddlewareFunc {
	return func(final http.Handler) http.Handler {
		// Apply middleware in reverse order so the first middleware wraps the outermost
		for i := len(middlewares) - 1; i >= 0; i-- {
			final = middlewares[i](final)
		}
		return final
	}
}

// Apply applies a middleware chain to an HTTP handler.
func Apply(handler http.Handler, middlewares []MiddlewareFunc) http.Handler {
	return Chain(middlewares...)(handler)
}

// Common middleware ordering recommendations
var (
	// DefaultMiddlewareOrder is the recommended order for general use
	DefaultMiddlewareOrder = []string{
		"request_id",
		"cors",
		"logging",
		"metrics",
		"auth",
		"rate_limit",
		"cache",
		"circuit_breaker",
	}

	// SecurityFirstOrder places security middleware at the earliest point
	SecurityFirstOrder = []string{
		"request_id",
		"cors",
		"auth",
		"rate_limit",
		"logging",
		"metrics",
		"cache",
		"circuit_breaker",
	}

	// PerformanceFirstOrder places caching before other middleware
	PerformanceFirstOrder = []string{
		"request_id",
		"cache",
		"cors",
		"auth",
		"rate_limit",
		"logging",
		"metrics",
		"circuit_breaker",
	}
)
