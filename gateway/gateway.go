// Package gateway implements the core API gateway functionality including
// reverse proxying, middleware orchestration, route matching, and request lifecycle management.
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
	"github.com/rajeshwarrao1253/high-performance-api-gateway/middleware"
	"github.com/rajeshwarrao1253/high-performance-api-gateway/upstream"
)

// Middleware is a function that wraps an HTTP handler with additional functionality.
type Middleware func(http.Handler) http.Handler

// Gateway is the core API gateway that handles all incoming requests.
type Gateway struct {
	config           *config.Config
	logger           *zap.Logger
	middlewareRegistry *middleware.Registry
	router           *Router
	loadBalancers    map[string]upstream.LoadBalancer
	healthCheckers   map[string]*upstream.HealthChecker
	proxyCache       map[string]*httputil.ReverseProxy
	ready            bool
	mu               sync.RWMutex
}

// New creates a new Gateway instance with the given configuration.
func New(cfg *config.Config, logger *zap.Logger, registry *middleware.Registry) *Gateway {
	gw := &Gateway{
		config:           cfg,
		logger:           logger,
		middlewareRegistry: registry,
		router:           NewRouter(cfg),
		loadBalancers:    make(map[string]upstream.LoadBalancer),
		healthCheckers:   make(map[string]*upstream.HealthChecker),
		proxyCache:       make(map[string]*httputil.ReverseProxy),
	}

	// Initialize load balancers and health checkers for each upstream
	gw.initializeUpstreams()

	gw.ready = true
	return gw
}

// initializeUpstreams creates load balancers and health checkers for all upstreams.
func (g *Gateway) initializeUpstreams() {
	for name, upstreamCfg := range g.config.Upstreams {
		// Extract target URLs
		targets := make([]upstream.Target, 0, len(upstreamCfg.Targets))
		for _, t := range upstreamCfg.Targets {
			targetURL, err := url.Parse(t.URL)
			if err != nil {
				g.logger.Error("invalid upstream target URL",
					zap.String("upstream", name),
					zap.String("url", t.URL),
					zap.Error(err),
				)
				continue
			}
			targets = append(targets, upstream.Target{
				URL:      targetURL,
				Weight:   t.Weight,
				Priority: t.Priority,
			})
		}

		// Create load balancer
		lb := upstream.NewLoadBalancer(upstreamCfg.Balancer, targets)
		g.loadBalancers[name] = lb

		// Create and start health checker if health check is configured
		if upstreamCfg.HealthCheck.Interval > 0 {
			hc := upstream.NewHealthChecker(
				name,
				targets,
				upstreamCfg.HealthCheck,
				g.logger,
				lb,
			)
			g.healthCheckers[name] = hc
			hc.Start()
			g.logger.Info("health checker started",
				zap.String("upstream", name),
				zap.String("interval", upstreamCfg.HealthCheck.Interval.String()),
			)
		}

		g.logger.Info("upstream initialized",
			zap.String("name", name),
			zap.String("balancer", upstreamCfg.Balancer),
			zap.Int("targets", len(targets)),
		)
	}
}

// Handler returns the HTTP handler for the gateway.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Match route
		route := g.router.Match(r.URL.Path, r.Method)
		if route == nil {
			g.logger.Warn("no route matched",
				zap.String("path", r.URL.Path),
				zap.String("method", r.Method),
			)
			http.Error(w, `{"error":"not found","message":"no route matches the request"}`, http.StatusNotFound)
			return
		}

		// Build middleware chain for this route
		handler := g.buildHandler(route)
		handler = g.applyMiddleware(handler, route)

		// Execute request
		handler.ServeHTTP(w, r)

		g.logger.Debug("request handled",
			zap.String("path", r.URL.Path),
			zap.String("method", r.Method),
			zap.String("upstream", route.Upstream),
			zap.Duration("duration", time.Since(start)),
		)
	})
}

// buildHandler creates the base HTTP handler for proxying requests to upstream.
func (g *Gateway) buildHandler(route *config.RouteConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get upstream load balancer
		lb, ok := g.loadBalancers[route.Upstream]
		if !ok {
			g.logger.Error("upstream not found",
				zap.String("upstream", route.Upstream),
			)
			http.Error(w, `{"error":"internal","message":"upstream not configured"}`, http.StatusInternalServerError)
			return
		}

		// Select target using load balancer
		target := lb.Next(r)
		if target == nil {
			g.logger.Error("no healthy targets available",
				zap.String("upstream", route.Upstream),
			)
			http.Error(w, `{"error":"service_unavailable","message":"no healthy upstream targets"}`, http.StatusServiceUnavailable)
			return
		}

		// Strip prefix if configured
		if route.StripPrefix != "" {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, route.StripPrefix)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
		}

		// Rewrite URL if pattern is configured
		if route.Rewrite.Pattern != "" && route.Rewrite.Replacement != "" {
			r.URL.Path = strings.Replace(r.URL.Path, route.Rewrite.Pattern, route.Rewrite.Replacement, 1)
		}

		// Add route-specific headers
		for key, value := range route.AddHeaders {
			r.Header.Set(key, value)
		}

		// Get or create reverse proxy for this target
		proxy := g.getProxy(target.URL.String(), route)

		// Update request URL to target
		originalHost := r.Host
		r.URL.Scheme = target.URL.Scheme
		r.URL.Host = target.URL.Host
		r.Host = target.URL.Host

		// Track request for least-connections balancer
		if done := lb.TrackRequest(target); done != nil {
			defer done()
		}

		// Execute proxy
		proxy.ServeHTTP(w, r)

		// Restore original host for logging
		r.Host = originalHost
	})
}

// getProxy returns a cached or newly created reverse proxy for the given target.
func (g *Gateway) getProxy(targetURL string, route *config.RouteConfig) *httputil.ReverseProxy {
	g.mu.RLock()
	proxy, ok := g.proxyCache[targetURL]
	g.mu.RUnlock()

	if ok {
		return proxy
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Double-check after acquiring write lock
	if proxy, ok := g.proxyCache[targetURL]; ok {
		return proxy
	}

	upstreamCfg := g.config.Upstreams[route.Upstream]

	proxy = httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "http",
		Host:   "placeholder",
	})

	// Custom transport with connection pooling
	transport := &http.Transport{
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		WriteBufferSize:     64 * 1024,
		ReadBufferSize:      64 * 1024,
		DisableCompression:  false,
	}
	proxy.Transport = transport

	// Custom error handler
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		g.logger.Error("proxy error",
			zap.String("target", targetURL),
			zap.String("path", r.URL.Path),
			zap.Error(err),
		)
		http.Error(w, fmt.Sprintf(`{"error":"gateway","message":"%s"}`, err.Error()), http.StatusBadGateway)
	}

	// Modify response to inject gateway headers
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set("X-Gateway", "high-performance-api-gateway")
		resp.Header.Set("X-Cache-Status", "MISS")
		return nil
	}

	// Set upstream timeout if configured
	if upstreamCfg.Timeout > 0 {
		transport.ResponseHeaderTimeout = upstreamCfg.Timeout
	}

	g.proxyCache[targetURL] = proxy
	return proxy
}

// applyMiddleware builds the middleware chain for a route.
func (g *Gateway) applyMiddleware(handler http.Handler, route *config.RouteConfig) http.Handler {
	// Apply middleware in reverse order so the first in the list executes first
	middlewares := g.middlewareRegistry.Resolve(route.Middleware)
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// Ready returns true when the gateway is ready to serve requests.
func (g *Gateway) Ready() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.ready
}

// Shutdown gracefully shuts down the gateway and its components.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	g.ready = false
	g.mu.Unlock()

	g.logger.Info("shutting down gateway")

	// Stop all health checkers
	var wg sync.WaitGroup
	for name, hc := range g.healthCheckers {
		wg.Add(1)
		go func(n string, checker *upstream.HealthChecker) {
			defer wg.Done()
			checker.Stop()
			g.logger.Info("health checker stopped", zap.String("upstream", n))
		}(name, hc)
	}

	// Wait for health checkers with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		g.logger.Info("all health checkers stopped")
	case <-ctx.Done():
		g.logger.Warn("health checker shutdown timed out")
	}

	return nil
}

// UpdateConfig hot-reloads the gateway configuration.
func (g *Gateway) UpdateConfig(cfg *config.Config) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.logger.Info("updating gateway configuration")

	// Shutdown existing health checkers
	for _, hc := range g.healthCheckers {
		hc.Stop()
	}

	// Update configuration
	g.config = cfg
	g.router = NewRouter(cfg)
	g.loadBalancers = make(map[string]upstream.LoadBalancer)
	g.healthCheckers = make(map[string]*upstream.HealthChecker)
	g.proxyCache = make(map[string]*httputil.ReverseProxy)

	// Reinitialize upstreams
	g.initializeUpstreams()

	g.logger.Info("gateway configuration updated")
	return nil
}

// GetRouter returns the gateway's router for testing purposes.
func (g *Gateway) GetRouter() *Router {
	return g.router
}

// GetLoadBalancer returns the load balancer for an upstream.
func (g *Gateway) GetLoadBalancer(name string) (upstream.LoadBalancer, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	lb, ok := g.loadBalancers[name]
	return lb, ok
}
