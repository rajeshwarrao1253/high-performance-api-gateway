# High-Performance API Gateway - Dockerfile
# Multi-stage build for minimal production image

# Stage 1: Build
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache ca-certificates git tzdata

# Set working directory
WORKDIR /build

# Copy go module files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary with optimizations
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags='-w -s -extldflags "-static"' \
    -a -installsuffix cgo \
    -o /build/api-gateway \
    main.go

# Stage 2: Minimal runtime image
FROM scratch

# Copy CA certificates for HTTPS
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy timezone data
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy the compiled binary
COPY --from=builder /build/api-gateway /api-gateway

# Copy default configuration
COPY --from=builder /build/config/gateway.yaml /etc/gateway/gateway.yaml

# Use non-root user (note: scratch doesn't have useradd, so we use a numeric ID)
USER 65534:65534

# Expose ports
EXPOSE 8080 9090

# Health check endpoint
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/api-gateway", "-health-check"]

# Set entrypoint
ENTRYPOINT ["/api-gateway"]
CMD ["-config", "/etc/gateway/gateway.yaml"]
