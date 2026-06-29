// Package main provides the entry point for the high-performance API gateway.
// It handles configuration loading, dependency initialization, server startup,
// and graceful shutdown with proper signal handling.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
	"github.com/rajeshwarrao1253/high-performance-api-gateway/gateway"
	"github.com/rajeshwarrao1253/high-performance-api-gateway/middleware"
)

const (
	shutdownTimeout = 30 * time.Second
	readTimeout     = 30 * time.Second
	writeTimeout    = 30 * time.Second
	idleTimeout     = 120 * time.Second
)

func main() {
	var (
		configPath = flag.String("config", "config/gateway.yaml", "Path to gateway configuration file")
		port       = flag.String("port", "", "Override server port")
	)
	flag.Parse()

	// Initialize structured logger
	logger := initLogger()
	defer logger.Sync()

	logger.Info("starting api gateway",
		zap.String("version", "1.0.0"),
		zap.String("config", *configPath),
	)

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal("failed to load configuration", zap.Error(err))
	}

	// Override port if specified via CLI
	serverPort := cfg.Server.Port
	if *port != "" {
		serverPort = *port
	}

	// Initialize Redis client
	redisClient := initRedis(cfg.Redis, logger)
	defer redisClient.Close()

	// Create middleware registry
	middlewareRegistry := middleware.NewRegistry(logger, redisClient, cfg)

	// Create and configure gateway
	gw := gateway.New(cfg, logger, middlewareRegistry)

	// Setup HTTP server with timeouts
	srv := &http.Server{
		Addr:         ":" + serverPort,
		Handler:      gw.Handler(),
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}

	// Start health check and metrics endpoints in background
	go startAuxiliaryServers(logger, cfg, gw)

	// Handle graceful shutdown in a separate goroutine
	idleConnsClosed := make(chan struct{})
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		sig := <-sigChan
		logger.Info("received shutdown signal",
			zap.String("signal", sig.String()),
		)

		// Create a deadline for graceful shutdown
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("server forced to shutdown", zap.Error(err))
		}

		close(idleConnsClosed)
	}()

	// Start the main server
	logger.Info("gateway server listening",
		zap.String("address", srv.Addr),
		zap.String("port", serverPort),
	)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal("server failed to start", zap.Error(err))
	}

	// Wait for graceful shutdown to complete
	<-idleConnsClosed
	logger.Info("gateway server stopped gracefully")
}

// initLogger creates a production-ready zap logger with JSON formatting.
func initLogger() *zap.Logger {
	config := zap.NewProductionConfig()
	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zap.ISO8601TimeEncoder

	logger, err := config.Build(zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))
	if err != nil {
		// Fallback to basic logger if zap fails to initialize
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\\n", err)
		os.Exit(1)
	}

	return logger
}

// initRedis creates a Redis client with connection pooling and health checks.
func initRedis(cfg config.RedisConfig, logger *zap.Logger) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		MaxRetries:   3,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	// Verify connectivity with a ping
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("redis connection failed, continuing without caching/rate limiting",
			zap.String("addr", cfg.Addr),
			zap.Error(err),
		)
	} else {
		logger.Info("redis connection established", zap.String("addr", cfg.Addr))
	}

	return client
}

// startAuxiliaryServers starts the health check and Prometheus metrics endpoints.
func startAuxiliaryServers(logger *zap.Logger, cfg *config.Config, gw *gateway.Gateway) {
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"healthy","service":"api-gateway"}`))
	})

	// Readiness check
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if gw.Ready() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ready"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"not ready"}`))
	})

	// Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())

	metricsPort := cfg.Server.MetricsPort
	if metricsPort == "" {
		metricsPort = "9090"
	}

	logger.Info("auxiliary server starting",
		zap.String("port", metricsPort),
		zap.Strings("endpoints", []string{"/health", "/ready", "/metrics"}),
	)

	server := &http.Server{
		Addr:    ":" + metricsPort,
		Handler: mux,
	}

	if err := server.ListenAndServe(); err != nil {
		logger.Error("auxiliary server error", zap.Error(err))
	}
}
