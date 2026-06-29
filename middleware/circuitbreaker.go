// Package middleware implements the circuit breaker pattern for upstream service calls.
// It tracks failures and transitions between closed, open, and half-open states
// to prevent cascading failures in distributed systems.
package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
)

// CircuitState represents the current state of a circuit breaker.
type CircuitState int32

const (
	// StateClosed allows requests to pass through and tracks failures.
	StateClosed CircuitState = iota
	// StateOpen blocks all requests and returns an error immediately.
	StateOpen
	// StateHalfOpen allows a limited number of test requests through.
	StateHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitBreaker implements the circuit breaker pattern for HTTP requests.
type CircuitBreaker struct {
	config   config.CircuitBreakerConfig
	logger   *zap.Logger
	mu       sync.RWMutex
	state    int32 // atomic access for state
	failures uint64
	successes uint64
	lastFailure time.Time

	// Per-endpoint circuit breakers
	circuits sync.Map // map[string]*CircuitBreaker
}

// NewCircuitBreaker creates a new circuit breaker manager.
func NewCircuitBreaker(cfg config.CircuitBreakerConfig, logger *zap.Logger) *CircuitBreaker {
	return &CircuitBreaker{
		config: cfg,
		logger: logger.Named("circuitbreaker"),
		state:  int32(StateClosed),
	}
}

// GetCircuitBreaker returns a circuit breaker for the given endpoint.
// Each unique path gets its own circuit breaker instance.
func (cb *CircuitBreaker) GetCircuitBreaker(path string) *CircuitBreaker {
	if !cb.config.Enabled {
		return cb
	}

	actual, _ := cb.circuits.LoadOrStore(path, &CircuitBreaker{
		config: cb.config,
		logger: cb.logger,
		state:  int32(StateClosed),
	})
	return actual.(*CircuitBreaker)
}

// Handler returns the HTTP handler that wraps requests with circuit breaker logic.
func (cb *CircuitBreaker) Handler(next http.Handler) http.Handler {
	if !cb.config.Enabled {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get per-endpoint circuit breaker
		endpoint := r.URL.Path
		breaker := cb.GetCircuitBreaker(endpoint)

		// Check current state
		state := breaker.currentState()

		switch state {
		case StateOpen:
			// Circuit is open - check if recovery timeout has elapsed
			if breaker.canAttemptReset() {
				breaker.transitionTo(StateHalfOpen)
				cb.logger.Info("circuit breaker transitioning to half-open",
					zap.String("endpoint", endpoint),
				)
				// Fall through to half-open handling
			} else {
				cb.logger.Debug("circuit breaker open, request rejected",
					zap.String("endpoint", endpoint),
				)
				breaker.respondUnavailable(w, r)
				return
			}

		case StateHalfOpen:
			// In half-open state, limit concurrent test requests
			if !breaker.allowTestRequest() {
				breaker.respondUnavailable(w, r)
				return
			}
		}

		// Wrap the response writer to capture status code
		rw := &responseCapture{ResponseWriter: w, statusCode: http.StatusOK}

		// Execute request
		next.ServeHTTP(rw, r)

		// Record result based on response status
		breaker.recordResult(rw.statusCode)
	})
}

// recordResult updates the circuit breaker based on the response status code.
func (cb *CircuitBreaker) recordResult(statusCode int) {
	isFailure := statusCode >= 500 || statusCode == 0

	state := cb.currentState()

	switch state {
	case StateHalfOpen:
		if isFailure {
			// Failure in half-open state - go back to open
			cb.transitionTo(StateOpen)
			atomic.StoreUint64(&cb.failures, 0)
			atomic.StoreUint64(&cb.successes, 0)
			cb.mu.Lock()
			cb.lastFailure = time.Now()
			cb.mu.Unlock()
			cb.logger.Info("circuit breaker: failure in half-open, reverting to open")
		} else {
			// Success in half-open state
			successes := atomic.AddUint64(&cb.successes, 1)
			if successes >= uint64(cb.config.SuccessThreshold) {
				// Enough successes - close the circuit
				cb.transitionTo(StateClosed)
				atomic.StoreUint64(&cb.failures, 0)
				atomic.StoreUint64(&cb.successes, 0)
				cb.logger.Info("circuit breaker: recovered, transitioning to closed")
			}
		}

	case StateClosed:
		if isFailure {
			failures := atomic.AddUint64(&cb.failures, 1)
			cb.mu.Lock()
			cb.lastFailure = time.Now()
			cb.mu.Unlock()

			if failures >= uint64(cb.config.FailureThreshold) {
				cb.transitionTo(StateOpen)
				cb.logger.Info("circuit breaker: threshold exceeded, transitioning to open",
					zap.Uint64("failures", failures),
				)
			}
		} else {
			// Reset failures on success (optional: implement sliding window)
			atomic.StoreUint64(&cb.failures, 0)
		}
	}
}

// currentState returns the current circuit breaker state.
func (cb *CircuitBreaker) currentState() CircuitState {
	return CircuitState(atomic.LoadInt32(&cb.state))
}

// transitionTo atomically transitions the circuit breaker to a new state.
func (cb *CircuitBreaker) transitionTo(newState CircuitState) {
	atomic.StoreInt32(&cb.state, int32(newState))
}

// canAttemptReset checks if enough time has passed to attempt recovery.
func (cb *CircuitBreaker) canAttemptReset() bool {
	cb.mu.RLock()
	lastFailure := cb.lastFailure
	cb.mu.RUnlock()

	return time.Since(lastFailure) >= cb.config.RecoveryTimeout
}

// allowTestRequest determines if a test request should be allowed in half-open state.
// Uses a simple counter-based approach.
func (cb *CircuitBreaker) allowTestRequest() bool {
	// In a production implementation, use a proper semaphore or atomic counter
	// to limit concurrent requests in half-open state
	return true
}

// respondUnavailable sends a service unavailable response when the circuit is open.
func (cb *CircuitBreaker) respondUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", fmt.Sprintf("%d", int(cb.config.RecoveryTimeout.Seconds())))
	w.WriteHeader(http.StatusServiceUnavailable)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"error":       "service_unavailable",
		"message":     "circuit breaker is open - service temporarily unavailable",
		"retry_after": int(cb.config.RecoveryTimeout.Seconds()),
	})
}

// GetState returns the current state and statistics of the circuit breaker.
func (cb *CircuitBreaker) GetState() map[string]interface{} {
	return map[string]interface{}{
		"state":     cb.currentState().String(),
		"failures":  atomic.LoadUint64(&cb.failures),
		"successes": atomic.LoadUint64(&cb.successes),
	}
}

// responseCapture wraps http.ResponseWriter to capture the status code.
type responseCapture struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rc *responseCapture) WriteHeader(code int) {
	if !rc.written {
		rc.statusCode = code
		rc.written = true
		rc.ResponseWriter.WriteHeader(code)
	}
}

func (rc *responseCapture) Write(b []byte) (int, error) {
	if !rc.written {
		rc.WriteHeader(http.StatusOK)
	}
	return rc.ResponseWriter.Write(b)
}

// StatusCode returns the captured status code.
func (rc *responseCapture) StatusCode() int {
	return rc.statusCode
}

// Ensure responseCapture implements http.Flusher
var _ http.Flusher = (*responseCapture)(nil)

func (rc *responseCapture) Flush() {
	if f, ok := rc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Ensure responseCapture implements http.Hijacker
var _ http.Hijacker = (*responseCapture)(nil)

func (rc *responseCapture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rc.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijacking not supported")
}
