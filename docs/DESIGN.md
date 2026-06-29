# API Gateway - Design Document

## Overview

This document describes the architecture, design decisions, middleware pipeline, and performance optimizations of the High-Performance API Gateway.

## Architecture

### Reverse Proxy Layer

The gateway acts as a reverse proxy, forwarding client requests to upstream backend services. Key design decisions:

- **Connection Pooling**: HTTP transports maintain persistent connections to upstreams, reducing TCP handshake overhead. Each upstream target gets a dedicated connection pool with configurable limits.
- **HTTP/2 Support**: Go's `net/http` package automatically negotiates HTTP/2 when supported by both client and upstream.
- **Request Lifecycle**: Each request passes through route matching, middleware chain execution, load balancing, and proxy forwarding.

### Middleware Pipeline

The middleware system uses a **chain of responsibility** pattern where each middleware can:
- Inspect/modify the request before passing it downstream
- Inspect/modify the response after the handler completes
- Short-circuit the chain (e.g., rate limit exceeded)

```
Request -> RequestID -> CORS -> Logging -> Metrics -> Auth -> RateLimit -> Cache -> CircuitBreaker -> Proxy -> Upstream
```

#### Middleware Ordering

The order of middleware is critical for correctness and performance:

1. **RequestID** (first) - Ensures all downstream middleware has access to request ID
2. **CORS** (early) - Handles preflight before auth/rate limiting
3. **Logging** (early) - Captures full request lifecycle duration
4. **Metrics** (early) - Measures total latency including middleware
5. **Auth** (before rate limiting) - Identifies the user for per-client rate limits
6. **RateLimit** (after auth) - Uses authenticated user identity
7. **Cache** (before proxy) - Avoids upstream calls when possible
8. **CircuitBreaker** (last) - Protects the actual upstream call

### Router Design

The router implements **priority-based matching** with the following rules:

1. **Specificity Priority**: Exact path matches > wildcard prefix matches > parameterized matches
2. **Method Restrictions**: Routes with specific methods match before catch-all routes
3. **Depth Scoring**: Deeper paths (more segments) have higher priority
4. **First Match Wins**: Routes are sorted by priority; the first match is used

### Load Balancing

Four algorithms are supported, each with different use cases:

| Algorithm | Use Case | Complexity |
|-----------|----------|------------|
| Round Robin | Even distribution, stateless services | O(1) |
| Least Connections | Variable request durations, long-lived connections | O(n) |
| Weighted | Heterogeneous backend capacity | O(n) |
| Random | Simple, no state | O(1) |

### Health Checking

Two complementary approaches:

- **Active Health Checks**: Periodic HTTP probes to each target. Configurable path, interval, timeout, and failure threshold.
- **Passive Health Checks**: Tracks 5xx errors from actual requests. No additional overhead.

### Caching Strategy

Response caching uses **Redis** for distributed caching across gateway instances:

- **Cache Key Generation**: SHA-256 hash of method, path, query, vary headers, and user ID
- **TTL-Based Expiration**: Configurable per-route TTL with Redis key expiration
- **Cache Invalidation**: Pattern-based invalidation via `X-Cache-Invalidate` header
- **Size Limits**: Maximum body size prevents caching large responses

### Rate Limiting

Sliding window rate limiting with Redis Lua scripts for atomic operations:

- **Per-Client Limiting**: Based on API key, user ID, or IP address
- **Per-Endpoint Limiting**: Separate limits per API endpoint
- **Burst Allowance**: Configurable burst size for traffic spikes
- **Fail-Open**: Allows requests if Redis is unavailable

### Circuit Breaker

Three-state circuit breaker pattern:

```
  Closed (normal) ----[failures > threshold]----> Open (blocking)
     ^                                                  |
     |                                                  |
     |--[successes > threshold]<-- Half-Open (testing) <-|
              [failure]-----------------------------------
```

- **Closed State**: Normal operation, tracking failures
- **Open State**: Blocking requests, returning 503
- **Half-Open State**: Allowing limited test requests

### Authentication

JWT-based authentication with:

- **HMAC (HS256/HS384/HS512)** and **RSA (RS256)** signature support
- **Claims Extraction**: User ID, roles, tenant ID from JWT
- **RBAC**: Role-based access control with route-level role requirements
- **Token Refresh**: Early warning when token is approaching expiry

## Performance Optimizations

### Connection Management
- Persistent HTTP connections with configurable pool sizes
- Separate pools per upstream target
- TCP connection reuse across requests

### Memory Optimization
- Reverse proxy instances are cached per target URL
- Response body buffering with size limits
- Efficient header manipulation with pre-allocated maps

### Concurrency
- Lock-free round-robin balancer using atomic operations
- Per-endpoint circuit breakers reduce lock contention
- RWMutex for config reads, exclusive lock only for updates

### Latency Reduction
- Cache responses to avoid upstream calls
- Short-circuit auth/rate-limit for known results
- Connection pooling eliminates TCP handshake
- HTTP/2 multiplexing for concurrent requests

## Scalability

### Horizontal Scaling
- Stateless design allows running multiple gateway instances
- Redis shared state for rate limiting and caching
- Kubernetes HPA for auto-scaling based on CPU/memory

### Vertical Scaling
- Connection pool tuning based on available CPU/memory
- Configurable worker pool sizes
- Buffer size optimization for target throughput

## Security Considerations

1. **Non-root container execution** in Kubernetes
2. **TLS termination** support (certificate configuration)
3. **JWT validation** with signature verification and expiry checks
4. **Rate limiting** prevents brute force and DoS attacks
5. **Request ID propagation** enables audit trails
6. **CORS protection** for cross-origin requests
7. **Input validation** on all configuration parameters

## Monitoring

### Metrics (Prometheus)
- `gateway_requests_total` - Counter with method, path, status, upstream labels
- `gateway_request_duration_seconds` - Histogram with configurable buckets
- `gateway_active_connections` - Gauge for current connections
- `gateway_errors_total` - Error counter by status code
- `gateway_cache_hits_total` - Cache hit/miss tracking
- `gateway_rate_limited_total` - Rate limit events
- `gateway_upstream_health` - Upstream target health status
- `gateway_circuit_breaker_state` - Circuit breaker state

### Logs (Structured JSON)
- Request details: method, path, status, duration, client IP
- Contextual fields: request_id, user_id, upstream
- Severity-based log levels with automatic escalation

### Health Endpoints
- `/health` - Liveness probe (always returns 200 if process is running)
- `/ready` - Readiness probe (returns 200 only when fully initialized)
- `/metrics` - Prometheus scrape endpoint
