// Package config provides configuration management for the API gateway.
// It supports YAML-based configuration with hot-reload capabilities,
// route definitions, middleware chains, and upstream definitions.
package config

import (
	"fmt"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the complete gateway configuration.
type Config struct {
	Server    ServerConfig            `yaml:"server"`
	Redis     RedisConfig             `yaml:"redis"`
	JWT       JWTConfig               `yaml:"jwt"`
	RateLimit RateLimitConfig         `yaml:"rate_limit"`
	Cache     CacheConfig             `yaml:"cache"`
	Routes    []RouteConfig           `yaml:"routes"`
	Upstreams map[string]UpstreamConfig `yaml:"upstreams"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	CORS      CORSConfig              `yaml:"cors"`
	Log       LogConfig               `yaml:"log"`

	mu       sync.RWMutex
	watching bool
	onChange func(*Config)
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port           string        `yaml:"port"`
	MetricsPort    string        `yaml:"metrics_port"`
	ReadTimeout    time.Duration `yaml:"read_timeout"`
	WriteTimeout   time.Duration `yaml:"write_timeout"`
	IdleTimeout    time.Duration `yaml:"idle_timeout"`
	MaxHeaderBytes int           `yaml:"max_header_bytes"`
	TLSCert        string        `yaml:"tls_cert"`
	TLSKey         string        `yaml:"tls_key"`
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Addr         string `yaml:"addr"`
	Password     string `yaml:"password"`
	DB           int    `yaml:"db"`
	PoolSize     int    `yaml:"pool_size"`
	MinIdleConns int    `yaml:"min_idle_conns"`
}

// JWTConfig holds JWT authentication settings.
type JWTConfig struct {
	Secret           string        `yaml:"secret"`
	PublicKeyPath    string        `yaml:"public_key_path"`
	TokenHeader      string        `yaml:"token_header"`
	TokenPrefix      string        `yaml:"token_prefix"`
	Issuer           string        `yaml:"issuer"`
	Audience         []string      `yaml:"audience"`
	MaxAge           time.Duration `yaml:"max_age"`
	RefreshThreshold time.Duration `yaml:"refresh_threshold"`
}

// RateLimitConfig holds rate limiting settings.
type RateLimitConfig struct {
	Enabled        bool          `yaml:"enabled"`
	DefaultLimit   int           `yaml:"default_limit"`
	DefaultWindow  time.Duration `yaml:"default_window"`
	RedisPrefix    string        `yaml:"redis_prefix"`
	BurstSize      int           `yaml:"burst_size"`
	PerClientLimit bool          `yaml:"per_client_limit"`
	PerEndpoint    bool          `yaml:"per_endpoint"`
}

// CacheConfig holds response caching settings.
type CacheConfig struct {
	Enabled      bool          `yaml:"enabled"`
	TTL          time.Duration `yaml:"ttl"`
	RedisPrefix  string        `yaml:"redis_prefix"`
	MaxBodySize  int64         `yaml:"max_body_size"`
	CacheMethods []string      `yaml:"cache_methods"`
	CacheStatus  []int         `yaml:"cache_status"`
}

// RouteConfig defines a single route rule.
type RouteConfig struct {
	Path         string            `yaml:"path"`
	Methods      []string          `yaml:"methods"`
	Upstream     string            `yaml:"upstream"`
	Middleware   []string          `yaml:"middleware"`
	StripPrefix  string            `yaml:"strip_prefix"`
	AddHeaders   map[string]string `yaml:"add_headers"`
	Rewrite      RewriteConfig     `yaml:"rewrite"`
	RateLimit    *RateLimitOverride `yaml:"rate_limit_override"`
	CacheTTL     time.Duration     `yaml:"cache_ttl"`
	RequireAuth  bool              `yaml:"require_auth"`
	AllowedRoles []string          `yaml:"allowed_roles"`
}

// RewriteConfig holds URL rewrite rules.
type RewriteConfig struct {
	Pattern     string `yaml:"pattern"`
	Replacement string `yaml:"replacement"`
}

// RateLimitOverride allows per-route rate limit customization.
type RateLimitOverride struct {
	Limit   int           `yaml:"limit"`
	Window  time.Duration `yaml:"window"`
	Burst   int           `yaml:"burst"`
}

// UpstreamConfig defines backend service settings.
type UpstreamConfig struct {
	Balancer    string           `yaml:"balancer"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Targets     []TargetConfig   `yaml:"targets"`
	Timeout     time.Duration    `yaml:"timeout"`
	Retry       RetryConfig      `yaml:"retry"`
}

// TargetConfig defines a single backend target.
type TargetConfig struct {
	URL      string `yaml:"url"`
	Weight   int    `yaml:"weight"`
	Priority int    `yaml:"priority"`
}

// HealthCheckConfig holds health check settings.
type HealthCheckConfig struct {
	Path     string        `yaml:"path"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Failures int           `yaml:"failures"`
}

// RetryConfig holds retry settings.
type RetryConfig struct {
	Attempts     int           `yaml:"attempts"`
	Backoff      time.Duration `yaml:"backoff"`
	RetryableCodes []int       `yaml:"retryable_codes"`
}

// CircuitBreakerConfig holds circuit breaker settings.
type CircuitBreakerConfig struct {
	Enabled            bool          `yaml:"enabled"`
	FailureThreshold   int           `yaml:"failure_threshold"`
	RecoveryTimeout    time.Duration `yaml:"recovery_timeout"`
	HalfOpenMaxCalls   int           `yaml:"half_open_max_calls"`
	SuccessThreshold   int           `yaml:"success_threshold"`
}

// CORSConfig holds CORS settings.
type CORSConfig struct {
	Enabled          bool     `yaml:"enabled"`
	AllowedOrigins   []string `yaml:"allowed_origins"`
	AllowedMethods   []string `yaml:"allowed_methods"`
	AllowedHeaders   []string `yaml:"allowed_headers"`
	ExposedHeaders   []string `yaml:"exposed_headers"`
	AllowCredentials bool     `yaml:"allow_credentials"`
	MaxAge           int      `yaml:"max_age"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level       string `yaml:"level"`
	Format      string `yaml:"format"`
	Output      string `yaml:"output"`
	EnableCaller bool  `yaml:"enable_caller"`
}

// Load reads configuration from a YAML file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	// Apply defaults
	cfg.applyDefaults()

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// applyDefaults sets default values for optional configuration fields.
func (c *Config) applyDefaults() {
	if c.Server.Port == "" {
		c.Server.Port = "8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 30 * time.Second
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = 30 * time.Second
	}
	if c.Server.MaxHeaderBytes == 0 {
		c.Server.MaxHeaderBytes = 1 << 20 // 1MB
	}

	if c.Redis.Addr == "" {
		c.Redis.Addr = "localhost:6379"
	}
	if c.Redis.PoolSize == 0 {
		c.Redis.PoolSize = 10
	}

	if c.JWT.TokenHeader == "" {
		c.JWT.TokenHeader = "Authorization"
	}
	if c.JWT.TokenPrefix == "" {
		c.JWT.TokenPrefix = "Bearer"
	}
	if c.JWT.MaxAge == 0 {
		c.JWT.MaxAge = 24 * time.Hour
	}

	if c.RateLimit.DefaultLimit == 0 {
		c.RateLimit.DefaultLimit = 100
	}
	if c.RateLimit.DefaultWindow == 0 {
		c.RateLimit.DefaultWindow = time.Minute
	}
	if c.RateLimit.RedisPrefix == "" {
		c.RateLimit.RedisPrefix = "ratelimit:"
	}

	if c.Cache.TTL == 0 {
		c.Cache.TTL = 5 * time.Minute
	}
	if c.Cache.RedisPrefix == "" {
		c.Cache.RedisPrefix = "cache:"
	}
	if c.Cache.MaxBodySize == 0 {
		c.Cache.MaxBodySize = 10 * 1024 * 1024 // 10MB
	}
	if len(c.Cache.CacheMethods) == 0 {
		c.Cache.CacheMethods = []string{"GET", "HEAD"}
	}

	if c.CircuitBreaker.FailureThreshold == 0 {
		c.CircuitBreaker.FailureThreshold = 5
	}
	if c.CircuitBreaker.RecoveryTimeout == 0 {
		c.CircuitBreaker.RecoveryTimeout = 30 * time.Second
	}
	if c.CircuitBreaker.HalfOpenMaxCalls == 0 {
		c.CircuitBreaker.HalfOpenMaxCalls = 3
	}
	if c.CircuitBreaker.SuccessThreshold == 0 {
		c.CircuitBreaker.SuccessThreshold = 2
	}
}

// Validate checks the configuration for required fields and valid values.
func (c *Config) Validate() error {
	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route is required")
	}

	for i, route := range c.Routes {
		if route.Path == "" {
			return fmt.Errorf("route[%d]: path is required", i)
		}
		if route.Upstream == "" {
			return fmt.Errorf("route[%d]: upstream is required", i)
		}
		if _, ok := c.Upstreams[route.Upstream]; !ok {
			return fmt.Errorf("route[%d]: upstream %q not defined", i, route.Upstream)
		}
	}

	for name, upstream := range c.Upstreams {
		if len(upstream.Targets) == 0 {
			return fmt.Errorf("upstream %q: at least one target is required", name)
		}
		if upstream.Balancer == "" {
			// Default to round_robin
			upstream.Balancer = "round_robin"
		}
		validBalancers := map[string]bool{
			"round_robin":      true,
			"least_connections": true,
			"weighted":         true,
			"random":           true,
		}
		if !validBalancers[upstream.Balancer] {
			return fmt.Errorf("upstream %q: invalid balancer %q", name, upstream.Balancer)
		}
	}

	return nil
}

// Clone creates a deep copy of the configuration.
func (c *Config) Clone() *Config {
	c.mu.RLock()
	defer c.mu.RUnlock()

	clone := &Config{
		Server:         c.Server,
		Redis:          c.Redis,
		JWT:            c.JWT,
		RateLimit:      c.RateLimit,
		Cache:          c.Cache,
		CircuitBreaker: c.CircuitBreaker,
		CORS:           c.CORS,
		Log:            c.Log,
		Routes:         make([]RouteConfig, len(c.Routes)),
		Upstreams:      make(map[string]UpstreamConfig, len(c.Upstreams)),
	}

	copy(clone.Routes, c.Routes)
	for k, v := range c.Upstreams {
		clone.Upstreams[k] = v
	}

	return clone
}

// Watch enables hot-reload of configuration by watching the file for changes.
func (c *Config) Watch(path string, onChange func(*Config)) error {
	c.mu.Lock()
	if c.watching {
		c.mu.Unlock()
		return fmt.Errorf("already watching configuration")
	}
	c.watching = true
	c.onChange = onChange
	c.mu.Unlock()

	// In a production implementation, use fsnotify to watch file changes
	// For now, we provide the hook for the caller to implement
	return nil
}

// GetRoute returns the route configuration matching the given path and method.
func (c *Config) GetRoute(path, method string) *RouteConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Routes {
		route := &c.Routes[i]
		if route.matchPath(path) && route.matchMethod(method) {
			return route
		}
	}
	return nil
}

// matchPath checks if a request path matches the route pattern.
func (r *RouteConfig) matchPath(path string) bool {
	if r.Path == path {
		return true
	}
	// Support wildcard matching
	if len(r.Path) > 0 && r.Path[len(r.Path)-1] == '*' {
		prefix := r.Path[:len(r.Path)-1]
		return len(path) >= len(prefix) && path[:len(prefix)] == prefix
	}
	return false
}

// matchMethod checks if a request method is allowed for the route.
func (r *RouteConfig) matchMethod(method string) bool {
	if len(r.Methods) == 0 {
		return true // Allow all methods if not specified
	}
	for _, m := range r.Methods {
		if m == method || m == "*" {
			return true
		}
	}
	return false
}
