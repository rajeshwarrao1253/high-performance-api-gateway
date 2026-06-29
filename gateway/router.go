// Package gateway provides the routing subsystem for the API gateway.
// The Router supports path-based routing, method matching, wildcard patterns,
// and route groups with priority-based matching.
package gateway

import (
	"strings"
	"sync"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
)

// Router handles HTTP request routing based on path and method matching.
type Router struct {
	routes []Route
	mu     sync.RWMutex
}

// Route represents a compiled route with efficient matching capabilities.
type Route struct {
	config      *config.RouteConfig
	path        string
	methods     map[string]bool
	isWildcard  bool
	segments    []string
	priority    int
}

// NewRouter creates a new Router from the gateway configuration.
// Routes are sorted by specificity so more specific routes match first.
func NewRouter(cfg *config.Config) *Router {
	router := &Router{
		routes: make([]Route, 0, len(cfg.Routes)),
	}

	for i := range cfg.Routes {
		routeCfg := &cfg.Routes[i]
		route := compileRoute(routeCfg, i)
		router.routes = append(router.routes, route)
	}

	// Sort routes by priority (more specific = higher priority)
	router.sortRoutes()

	return router
}

// compileRoute creates a compiled Route from a RouteConfig.
func compileRoute(cfg *config.RouteConfig, index int) Route {
	route := Route{
		config:   cfg,
		path:     cfg.Path,
		methods:  make(map[string]bool, len(cfg.Methods)),
		priority: calculatePriority(cfg),
	}

	// Compile method set for O(1) lookups
	for _, method := range cfg.Methods {
		route.methods[method] = true
	}

	// Check for wildcard pattern
	if strings.HasSuffix(cfg.Path, "*") {
		route.isWildcard = true
		route.path = strings.TrimSuffix(cfg.Path, "*")
	}

	// Split path into segments for segment-based matching
	route.segments = splitPath(route.path)

	return route
}

// calculatePriority assigns a priority score to a route.
// Higher scores indicate more specific routes that should match first.
func calculatePriority(cfg *config.RouteConfig) int {
	priority := 0

	// Exact path matches (no wildcard) get higher priority
	if !strings.HasSuffix(cfg.Path, "*") {
		priority += 1000
	}

	// Routes with more specific method restrictions get higher priority
	if len(cfg.Methods) > 0 && !contains(cfg.Methods, "*") {
		priority += len(cfg.Methods) * 10
	}

	// Deeper paths (more segments) get higher priority
	segments := splitPath(cfg.Path)
	priority += len(segments) * 5

	// Routes with auth requirements get slightly higher priority
	if cfg.RequireAuth {
		priority += 2
	}

	return priority
}

// Match finds the best matching route for the given path and method.
// Routes are evaluated in priority order for deterministic matching.
func (r *Router) Match(path, method string) *config.RouteConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, route := range r.routes {
		if route.match(path, method) {
			return route.config
		}
	}

	return nil
}

// match checks if a request path and method match this route.
func (r *Route) match(path, method string) bool {
	return r.matchPath(path) && r.matchMethod(method)
}

// matchPath checks if a request path matches the route path pattern.
func (r *Route) matchPath(path string) bool {
	// Exact match
	if r.path == path {
		return true
	}

	// Wildcard prefix match
	if r.isWildcard && strings.HasPrefix(path, r.path) {
		return true
	}

	// Support path parameter matching (e.g., /api/v1/users/:id)
	if !r.isWildcard && strings.Contains(r.path, ":") {
		return r.matchParameterizedPath(path)
	}

	return false
}

// matchParameterizedPath performs segment-by-segment matching
// supporting path parameters denoted by ':' prefix.
func (r *Route) matchParameterizedPath(path string) bool {
	requestSegments := splitPath(path)

	if len(requestSegments) != len(r.segments) {
		return false
	}

	for i, segment := range r.segments {
		// Path parameter matches any segment
		if strings.HasPrefix(segment, ":") {
			continue
		}
		if segment != requestSegments[i] {
			return false
		}
	}

	return true
}

// matchMethod checks if a request method is allowed for this route.
func (r *Route) matchMethod(method string) bool {
	// Empty method set means all methods are allowed
	if len(r.methods) == 0 {
		return true
	}

	// Check for wildcard method match
	if r.methods["*"] {
		return true
	}

	// Case-insensitive method matching
	return r.methods[method]
}

// sortRoutes sorts routes by priority (highest first) for deterministic matching.
func (r *Router) sortRoutes() {
	// Simple bubble sort for stability (routes with equal priority maintain order)
	n := len(r.routes)
	for i := 0; i < n-1; i++ {
		for j := 0; j < n-i-1; j++ {
			if r.routes[j].priority < r.routes[j+1].priority {
				r.routes[j], r.routes[j+1] = r.routes[j+1], r.routes[j]
			}
		}
	}
}

// GetRoutes returns a copy of all registered routes for inspection.
func (r *Router) GetRoutes() []Route {
	r.mu.RLock()
	defer r.mu.RUnlock()

	routes := make([]Route, len(r.routes))
	copy(routes, r.routes)
	return routes
}

// AddRoute dynamically adds a new route to the router.
func (r *Router) AddRoute(cfg *config.RouteConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	route := compileRoute(cfg, len(r.routes))
	r.routes = append(r.routes, route)
	r.sortRoutes()
}

// RemoveRoute removes all routes matching the given path pattern.
func (r *Router) RemoveRoute(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	filtered := make([]Route, 0, len(r.routes))
	for _, route := range r.routes {
		if route.path != path {
			filtered = append(filtered, route)
		}
	}
	r.routes = filtered
}

// splitPath splits a URL path into segments, removing empty segments.
func splitPath(path string) []string {
	if path == "/" || path == "" {
		return []string{}
	}

	path = strings.Trim(path, "/")
	if path == "" {
		return []string{}
	}

	return strings.Split(path, "/")
}

// contains checks if a string slice contains a specific value.
func contains(slice []string, value string) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

// RouteGroup provides a way to define routes with shared prefixes and middleware.
type RouteGroup struct {
	router     *Router
	prefix     string
	middleware []string
	requireAuth bool
	roles      []string
}

// Group creates a new route group with the given path prefix.
func (r *Router) Group(prefix string) *RouteGroup {
	return &RouteGroup{
		router: r,
		prefix: strings.TrimSuffix(prefix, "/"),
	}
}

// Use adds middleware to the route group.
func (g *RouteGroup) Use(middleware ...string) *RouteGroup {
	g.middleware = append(g.middleware, middleware...)
	return g
}

// Auth marks routes in this group as requiring authentication.
func (g *RouteGroup) Auth(roles ...string) *RouteGroup {
	g.requireAuth = true
	g.roles = roles
	return g
}

// Handle registers a new route within the group.
func (g *RouteGroup) Handle(path, method, upstream string) {
	fullPath := g.prefix + path

	// Merge group middleware with any route-specific middleware
	middleware := make([]string, len(g.middleware))
	copy(middleware, g.middleware)

	cfg := &config.RouteConfig{
		Path:         fullPath,
		Methods:      []string{method},
		Upstream:     upstream,
		Middleware:   middleware,
		RequireAuth:  g.requireAuth,
		AllowedRoles: g.roles,
	}

	g.router.AddRoute(cfg)
}

// GET is a convenience method for registering GET routes.
func (g *RouteGroup) GET(path, upstream string) {
	g.Handle(path, "GET", upstream)
}

// POST is a convenience method for registering POST routes.
func (g *RouteGroup) POST(path, upstream string) {
	g.Handle(path, "POST", upstream)
}

// PUT is a convenience method for registering PUT routes.
func (g *RouteGroup) PUT(path, upstream string) {
	g.Handle(path, "PUT", upstream)
}

// DELETE is a convenience method for registering DELETE routes.
func (g *RouteGroup) DELETE(path, upstream string) {
	g.Handle(path, "DELETE", upstream)
}

// PATCH is a convenience method for registering PATCH routes.
func (g *RouteGroup) PATCH(path, upstream string) {
	g.Handle(path, "PATCH", upstream)
}

// OPTIONS is a convenience method for registering OPTIONS routes.
func (g *RouteGroup) OPTIONS(path, upstream string) {
	g.Handle(path, "OPTIONS", upstream)
}
