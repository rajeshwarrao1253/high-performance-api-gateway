// Package middleware implements response caching using Redis as the backend.
// It supports cache-key generation, configurable TTL, cache invalidation,
// and conditional caching based on request methods and response status codes.
package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
)

// CachedResponse represents a cached HTTP response.
type CachedResponse struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       []byte            `json:"body"`
	CachedAt   time.Time         `json:"cached_at"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

// CacheMiddleware implements HTTP response caching.
type CacheMiddleware struct {
	config   config.CacheConfig
	logger   *zap.Logger
	redis    *redis.Client
	statuses map[int]bool
}

// NewCacheMiddleware creates a new caching middleware.
func NewCacheMiddleware(cfg config.CacheConfig, logger *zap.Logger, redisClient *redis.Client) *CacheMiddleware {
	cm := &CacheMiddleware{
		config:   cfg,
		logger:   logger.Named("cache"),
		redis:    redisClient,
		statuses: make(map[int]bool, len(cfg.CacheStatus)),
	}

	// Build lookup set for cacheable status codes
	for _, code := range cfg.CacheStatus {
		cm.statuses[code] = true
	}

	return cm
}

// Handler returns the HTTP handler that implements response caching.
func (cm *CacheMiddleware) Handler(next http.Handler) http.Handler {
	if !cm.config.Enabled || cm.redis == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only cache configured methods (default: GET, HEAD)
		if !cm.isCacheableMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		// Handle cache invalidation requests
		if r.Method == http.MethodDelete && r.Header.Get("X-Cache-Invalidate") != "" {
			cm.invalidateCache(r)
			next.ServeHTTP(w, r)
			return
		}

		// Generate cache key from request
		cacheKey := cm.buildCacheKey(r)

		// Try to serve from cache
		cached, err := cm.getCachedResponse(r.Context(), cacheKey)
		if err == nil && cached != nil {
			cm.logger.Debug("cache hit",
				zap.String("key", cacheKey),
				zap.String("path", r.URL.Path),
			)
			cm.serveCachedResponse(w, cached)
			return
		}

		// Cache miss - capture response
		cm.logger.Debug("cache miss",
			zap.String("key", cacheKey),
			zap.String("path", r.URL.Path),
			zap.Error(err),
		)

		capture := &cacheCapture{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(capture, r)

		// Cache the response if it's cacheable
		if cm.shouldCache(capture.statusCode) && capture.body != nil {
			ttl := cm.getTTL(r)
			cached := &CachedResponse{
				StatusCode: capture.statusCode,
				Headers:    cm.extractHeaders(capture.Header()),
				Body:       capture.body.Bytes(),
				CachedAt:   time.Now(),
				ExpiresAt:  time.Now().Add(ttl),
			}

			if err := cm.storeCache(r.Context(), cacheKey, cached, ttl); err != nil {
				cm.logger.Warn("failed to cache response",
					zap.String("key", cacheKey),
					zap.Error(err),
				)
			}
		}
	})
}

// buildCacheKey generates a deterministic cache key from the request.
func (cm *CacheMiddleware) buildCacheKey(r *http.Request) string {
	var parts []string
	parts = append(parts, cm.config.RedisPrefix)
	parts = append(parts, r.Method)
	parts = append(parts, r.URL.Path)
	parts = append(parts, r.URL.RawQuery)

	// Include relevant headers in cache key (e.g., Accept, Accept-Language)
	varyHeaders := []string{"Accept", "Accept-Language", "Accept-Encoding", "Authorization"}
	for _, header := range varyHeaders {
		value := r.Header.Get(header)
		if value != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", header, value))
		}
	}

	// Include user ID for authenticated requests to prevent cache leaking
	if userID := GetUserID(r.Context()); userID != "" {
		parts = append(parts, "uid="+userID)
	}

	rawKey := strings.Join(parts, "|")

	// Hash the key to ensure consistent length
	hash := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(hash[:16]) // Use first 16 bytes for shorter key
}

// getCachedResponse retrieves a cached response from Redis.
func (cm *CacheMiddleware) getCachedResponse(ctx context.Context, key string) (*CachedResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	data, err := cm.redis.Get(ctx, key).Bytes()
	if err != nil {
		return nil, err
	}

	var cached CachedResponse
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}

	// Check if cache has expired (Redis TTL should handle this, but double-check)
	if time.Now().After(cached.ExpiresAt) {
		cm.redis.Del(ctx, key)
		return nil, fmt.Errorf("cache expired")
	}

	return &cached, nil
}

// storeCache stores a response in Redis with the given TTL.
func (cm *CacheMiddleware) storeCache(ctx context.Context, key string, cached *CachedResponse, ttl time.Duration) error {
	// Check max body size
	if int64(len(cached.Body)) > cm.config.MaxBodySize {
		return fmt.Errorf("response body exceeds max cache size: %d > %d", len(cached.Body), cm.config.MaxBodySize)
	}

	data, err := json.Marshal(cached)
	if err != nil {
		return fmt.Errorf("marshal cached response: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	return cm.redis.Set(ctx, key, data, ttl).Err()
}

// invalidateCache removes cached responses matching the given pattern.
func (cm *CacheMiddleware) invalidateCache(r *http.Request) {
	pattern := r.Header.Get("X-Cache-Invalidate")
	if pattern == "" {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Find and delete matching keys
	iter := cm.redis.Scan(ctx, 0, cm.config.RedisPrefix+"*", 100).Iterator()
	var deleted int
	for iter.Next(ctx) {
		key := iter.Val()
		if strings.Contains(key, pattern) {
			cm.redis.Del(ctx, key)
			deleted++
		}
	}

	cm.logger.Info("cache invalidation completed",
		zap.String("pattern", pattern),
		zap.Int("deleted", deleted),
	)
}

// serveCachedResponse writes a cached response to the client.
func (cm *CacheMiddleware) serveCachedResponse(w http.ResponseWriter, cached *CachedResponse) {
	// Set cached headers
	for key, value := range cached.Headers {
		w.Header().Set(key, value)
	}

	// Mark as cache hit
	w.Header().Set("X-Cache-Status", "HIT")
	w.Header().Set("X-Cache-At", cached.CachedAt.Format(time.RFC3339))

	w.WriteHeader(cached.StatusCode)
	w.Write(cached.Body)
}

// shouldCache determines if a response with the given status code should be cached.
func (cm *CacheMiddleware) shouldCache(statusCode int) bool {
	return cm.statuses[statusCode]
}

// isCacheableMethod checks if the request method supports caching.
func (cm *CacheMiddleware) isCacheableMethod(method string) bool {
	for _, m := range cm.config.CacheMethods {
		if m == method {
			return true
		}
	}
	return false
}

// getTTL returns the TTL for caching a request, checking for route-specific overrides.
func (cm *CacheMiddleware) getTTL(r *http.Request) time.Duration {
	if ttl, ok := r.Context().Value("cache_ttl").(time.Duration); ok && ttl > 0 {
		return ttl
	}
	return cm.config.TTL
}

// extractHeaders extracts relevant headers for caching.
func (cm *CacheMiddleware) extractHeaders(header http.Header) map[string]string {
	headers := make(map[string]string)
	cacheableHeaders := []string{
		"Content-Type",
		"Content-Language",
		"Cache-Control",
		"Expires",
		"ETag",
		"Last-Modified",
	}

	for _, key := range cacheableHeaders {
		if value := header.Get(key); value != "" {
			headers[key] = value
		}
	}

	return headers
}

// cacheCapture wraps http.ResponseWriter to capture the response body.
type cacheCapture struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
	header     http.Header
	written    bool
}

func newCacheCapture(w http.ResponseWriter) *cacheCapture {
	return &cacheCapture{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		body:           &bytes.Buffer{},
		header:         w.Header().Clone(),
	}
}

func (cc *cacheCapture) WriteHeader(code int) {
	if !cc.written {
		cc.statusCode = code
		cc.written = true
		cc.ResponseWriter.WriteHeader(code)
	}
}

func (cc *cacheCapture) Write(b []byte) (int, error) {
	if !cc.written {
		cc.WriteHeader(http.StatusOK)
	}
	if cc.body != nil {
		cc.body.Write(b)
	}
	return cc.ResponseWriter.Write(b)
}

func (cc *cacheCapture) Header() http.Header {
	return cc.ResponseWriter.Header()
}

// Flush implements http.Flusher.
func (cc *cacheCapture) Flush() {
	if f, ok := cc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// cacheStats holds cache statistics for monitoring.
type cacheStats struct {
	Hits       uint64
	Misses     uint64
	Stores     uint64
	Errors     uint64
	Invalidations uint64
}

// Purge purges all cached responses.
func (cm *CacheMiddleware) Purge(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	iter := cm.redis.Scan(ctx, 0, cm.config.RedisPrefix+"*", 1000).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}

	if len(keys) > 0 {
		return cm.redis.Del(ctx, keys...).Err()
	}
	return nil
}
