# High-Performance API Gateway

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go" />
  <img src="https://img.shields.io/badge/Redis-7.0+-DC382D?style=for-the-badge&logo=redis&logoColor=white" alt="Redis" />
  <img src="https://img.shields.io/badge/Docker-2496ED?style=for-the-badge&logo=docker&logoColor=white" alt="Docker" />
  <img src="https://img.shields.io/badge/Kubernetes-326CE5?style=for-the-badge&logo=kubernetes&logoColor=white" alt="Kubernetes" />
  <img src="https://img.shields.io/badge/License-MIT-green?style=for-the-badge" alt="License" />
</p>

<p align="center">
  <b>A production-grade, high-performance API gateway built in Go</b><br/>
  Reverse proxy | Middleware pipeline | Dynamic rate limiting | JWT auth | Circuit breaker | Load balancing
</p>

---

## Features

| Feature | Description |
|---------|-------------|
| **JWT/OAuth2 Auth** | Stateless JWT validation with claims extraction and RBAC support |
| **Sliding Window Rate Limiting** | Redis-backed per-client and per-endpoint rate limiting |
| **Circuit Breaker** | Automatic failure detection with closed/open/half-open states |
| **Load Balancing** | Round-robin, least-connections, and weighted algorithms |
| **Response Caching** | Redis-backed caching with configurable TTL and cache invalidation |
| **Request/Response Transformation** | Header injection, body modification, path rewriting |
| **Request ID Propagation** | Distributed tracing with unique request IDs |
| **Structured Logging** | JSON-formatted logs with contextual fields |
| **Prometheus Metrics** | Request counts, latency histograms, active connections |
| **Graceful Shutdown** | Zero-downtime deployments with connection draining |

## Architecture

```
                    +------------------+
    Client   -----> |   API Gateway    |
                    +------------------+
                           |
           +---------------+---------------+
           |               |               |
    +------v------+ +------v------+ +------v------+
    |  Middleware | |  Middleware | |  Middleware |
    |   (Auth)    | | (Rate Limit)| |  (Logging)  |
    +------+------+ +------+------+ +------+------+
           |               |               |
           +---------------+---------------+
                           |
                    +------v------+
                    |   Router    |
                    +------+------+
                           |
              +------------+------------+
              |            |            |
    +---------v--+  +------v-----+  +----v---------+
    | Upstream 1 |  | Upstream 2 |  | Upstream 3   |
    +------------+  +------------+  +--------------+
```

### Core Components

- **Reverse Proxy** - HTTP/1.1 and HTTP/2 support with connection pooling
- **Middleware Chain** - Composable middleware with ordered execution
- **Plugin System** - Extensible middleware via configuration
- **Config Hot-Reload** - Zero-downtime configuration updates

## Benchmarks

```
BenchmarkDirectUpstream-8        52134 ops/sec    p50: 1.2ms    p99: 4.8ms
BenchmarkGatewayProxy-8          48792 ops/sec    p50: 1.5ms    p99: 5.2ms
BenchmarkWithMiddleware-8        42341 ops/sec    p50: 1.8ms    p99: 6.1ms
BenchmarkRateLimited-8           31245 ops/sec    p50: 2.1ms    p99: 7.3ms
BenchmarkCachedResponse-8        52103 ops/sec    p50: 0.8ms    p99: 3.2ms
```

*Tested on: 8 vCPU, 32GB RAM, Go 1.21, Redis 7*

## Quick Start

### Prerequisites

- Go 1.21+
- Redis 7+
- Docker & Docker Compose (optional)

### Local Development

```bash
# Clone the repository
git clone https://github.com/rajeshwarrao1253/high-performance-api-gateway.git
cd high-performance-api-gateway

# Install dependencies
go mod download

# Start Redis
docker run -d -p 6379:6379 redis:7-alpine

# Run the gateway
go run main.go

# Test the gateway
curl http://localhost:8080/api/v1/health
```

### Docker Compose

```bash
docker-compose up -d

# Gateway:    http://localhost:8080
# Prometheus: http://localhost:9090
# Grafana:    http://localhost:3000
```

## Configuration

The gateway uses YAML configuration with hot-reload support:

```yaml
server:
  port: 8080
  read_timeout: 30s
  write_timeout: 30s
  max_header_bytes: 1048576

routes:
  - path: /api/v1/users
    methods: [GET, POST]
    upstream: user_service
    middleware:
      - auth
      - rate_limit
      - cache

upstreams:
  user_service:
    balancer: round_robin
    health_check:
      path: /health
      interval: 10s
    targets:
      - url: http://localhost:8081
        weight: 1
      - url: http://localhost:8082
        weight: 2
```

## API Documentation

### Authentication

```bash
# Request with JWT token
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/protected
```

### Rate Limit Headers

```
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 99
X-RateLimit-Reset: 1640995200
```

## Monitoring

| Metric | Endpoint |
|--------|----------|
| Prometheus | `/metrics` |
| Health | `/health` |
| Ready | `/ready` |

## Deployment

### Kubernetes

```bash
kubectl apply -f k8s/
```

### Docker

```bash
docker build -t api-gateway .
docker run -p 8080:8080 api-gateway
```

## Project Structure

```
.
├── gateway/           # Core gateway and router
├── middleware/        # Middleware implementations
├── upstream/          # Load balancing and health checks
├── config/            # Configuration management
├── k8s/               # Kubernetes manifests
├── prometheus/        # Prometheus configuration
├── grafana/           # Grafana dashboards
├── docs/              # Documentation
├── main.go            # Entry point
├── Dockerfile         # Container image
└── docker-compose.yml # Local stack
```

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'feat: add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License.
