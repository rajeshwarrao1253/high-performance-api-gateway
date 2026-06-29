// Package middleware implements the sliding window rate limiter using Redis
// for distributed rate limiting across multiple gateway instances.
package middleware

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
)

const (
	slidingWindowScript = `
		local key = KEYS[1]
		local window = tonumber(ARGV[1])
		local limit = tonumber(ARGV[2])
		local now = tonumber(ARGV[3])
		local member = ARGV[4]

		-- Remove entries outside the current window
		redis.call('ZREMRANGEBYSCORE', key, 0, now - window)

		-- Count entries in current window
		local count = redis.call('ZCARD', key)

		-- Check if adding this request would exceed the limit
		if count >= limit then
			return {0, count, now + window}
		end

		-- Add the current request
		redis.call('ZADD', key, now, member)
		redis.call('EXPIRE', key, math.ceil(window / 1000))

		return {1, count + 1, now + window}
	`
)

// RateLimitMiddleware implements distributed sliding window rate limiting.
type RateLimitMiddleware struct {
	config    config.RateLimitConfig
	logger    *zap.Logger
	redis     *redis.Client
	scriptSHA string
}

// NewRateLimitMiddleware creates a new rate limiting middleware.
func NewRateLimitMiddleware(cfg config.RateLimitConfig, logger *zap.Logger, redisClient *redis.Client) *RateLimitMiddleware {
	rl := &RateLimitMiddleware{
		config: cfg,
		logger: logger.Named("ratelimit"),
		redis:  redisClient,
	}

	// Load Lua script into Redis for atomic operations
	if redisClient != nil {
		ctx, cancel := createTimeoutContext(5 * time.Second)
		defer cancel()

		sha, err := redisClient.ScriptLoad(ctx, slidingWindowScript).Result()
		if err != nil {
			logger.Warn("failed to load rate limit script, falling back to direct implementation",
				zap.Error(err),
			)
		} else {
			rl.scriptSHA = sha
		}
	}

	return rl
}

// Handler returns the HTTP handler that enforces rate limits.
func (rl *RateLimitMiddleware) Handler(next http.Handler) http.Handler {
	if !rl.config.Enabled {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Generate rate limit key based on configuration
		key := rl.buildKey(r)
		limit := rl.getLimit(r)
		window := rl.getWindow(r)
		burst := rl.getBurst(r)

		// Check rate limit
		allowed, remaining, resetTime, err := rl.checkLimit(r, key, limit, window, burst)
		if err != nil {
			rl.logger.Error("rate limit check failed",
				zap.String("key", key),
				zap.Error(err),
			)
			// Fail open - allow request on Redis error
			next.ServeHTTP(w, r)
			return
		}

		// Set rate limit headers
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetTime, 10))

		if !allowed {
			rl.logger.Debug("rate limit exceeded",
				zap.String("key", key),
				zap.String("path", r.URL.Path),
				zap.Int("limit", limit),
			)
			http.Error(w,
				fmt.Sprintf(`{"error":"too_many_requests","message":"rate limit exceeded: %d requests per %s","retry_after":%d}`,
					limit, window, resetTime-time.Now().Unix()),
				http.StatusTooManyRequests,
			)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// checkLimit performs the rate limit check using Redis sliding window.
func (rl *RateLimitMiddleware) checkLimit(r *http.Request, key string, limit int, window time.Duration, burst int) (bool, int, int64, error) {
	// Fallback if Redis is not available - allow request
	if rl.redis == nil {
		return true, limit, time.Now().Add(window).Unix(), nil
	}

	ctx, cancel := createTimeoutContext(3 * time.Second)
	defer cancel()

	now := time.Now().UnixMilli()
	windowMs := window.Milliseconds()
	member := fmt.Sprintf("%s:%d", r.Method, now)

	var result []interface{}
	var err error

	// Use Lua script if loaded, otherwise fallback to pipeline
	if rl.scriptSHA != "" {
		result, err = rl.redis.EvalSha(ctx, rl.scriptSHA, []string{key}, windowMs, limit+burst, now, member).Result()
	} else {
		result, err = rl.fallbackCheckLimit(ctx, key, limit+burst, windowMs, now, member)
	}

	if err != nil {
		return true, limit, now + windowMs/1000, err
	}

	results := result.([]interface{})
	allowed := results[0].(int64) == 1
	current := int(results[1].(int64))
	resetAt := results[2].(int64)

	remaining := limit - current
	if remaining < 0 {
		remaining = 0
	}

	return allowed, remaining, resetAt / 1000, nil
}

// fallbackCheckLimit implements rate limiting using Redis pipeline when Lua is unavailable.
func (rl *RateLimitMiddleware) fallbackCheckLimit(ctx context.Context, key string, limit int, windowMs, now int64, member string) ([]interface{}, error) {
	pipe := rl.redis.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "0", strconv.FormatInt(now-windowMs, 10))
	pipe.ZCard(ctx, key)
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: member})
	pipe.Expire(ctx, key, time.Duration(windowMs)*time.Millisecond)

	results, err := pipe.Exec(ctx)
	if err != nil {
		return nil, err
	}

	count := results[1].(*redis.IntCmd).Val()
	if count > int64(limit) {
		// Remove the just-added member since limit exceeded
		pipe := rl.redis.Pipeline()
		pipe.ZRem(ctx, key, member)
		pipe.Exec(ctx)
		return []interface{}{int64(0), count, now + windowMs}, nil
	}

	return []interface{}{int64(1), count, now + windowMs}, nil
}

// buildKey generates a rate limit key based on client identity and endpoint.
func (rl *RateLimitMiddleware) buildKey(r *http.Request) string {
	parts := []string{rl.config.RedisPrefix}

	// Add endpoint-specific key if enabled
	if rl.config.PerEndpoint {
		parts = append(parts, r.URL.Path)
	}

	// Add client-specific key if enabled
	if rl.config.PerClientLimit {
		identifier := rl.getClientIdentifier(r)
		parts = append(parts, identifier)
	}

	return strings.Join(parts, ":")
}

// getClientIdentifier extracts a client identifier from the request.
// It checks API key, authenticated user, and IP address in that order.
func (rl *RateLimitMiddleware) getClientIdentifier(r *http.Request) string {
	// Check for API key
	apiKey := r.Header.Get("X-API-Key")
	if apiKey != "" {
		return "apikey:" + hashString(apiKey)
	}

	// Check for authenticated user
	userID := GetUserID(r.Context())
	if userID != "" {
		return "user:" + userID
	}

	// Fall back to IP address
	ip := rl.getClientIP(r)
	return "ip:" + ip
}

// getClientIP extracts the real client IP from the request.
func (rl *RateLimitMiddleware) getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		// Use the first IP in the chain
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	// Check X-Real-IP header
	xri := r.Header.Get("X-Real-IP")
	if xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// getLimit returns the rate limit for the given request.
func (rl *RateLimitMiddleware) getLimit(r *http.Request) int {
	// Check for route-specific override via context
	if limit, ok := r.Context().Value("rate_limit").(int); ok && limit > 0 {
		return limit
	}
	return rl.config.DefaultLimit
}

// getWindow returns the rate limit window for the given request.
func (rl *RateLimitMiddleware) getWindow(r *http.Request) time.Duration {
	if window, ok := r.Context().Value("rate_limit_window").(time.Duration); ok && window > 0 {
		return window
	}
	return rl.config.DefaultWindow
}

// getBurst returns the burst size for the given request.
func (rl *RateLimitMiddleware) getBurst(r *http.Request) int {
	if burst, ok := r.Context().Value("rate_limit_burst").(int); ok && burst > 0 {
		return burst
	}
	return rl.config.BurstSize
}

// hashString creates a short hash of a string for use in Redis keys.
func hashString(s string) string {
	// Simple hash for key generation
	h := uint64(0)
	for _, c := range s {
		h = h*31 + uint64(c)
	}
	return fmt.Sprintf("%x", h%1000000007)
}
