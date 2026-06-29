// Package upstream provides health checking for upstream targets.
// It supports both active health checks (periodic HTTP pings) and passive
// health checks (tracking failures from actual requests).
package upstream

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
	"github.com/rajeshwarrao1253/high-performance-api-gateway/middleware"
)

// HealthChecker performs active health checks on upstream targets.
type HealthChecker struct {
	upstream    string
	targets     []*Target
	config      config.HealthCheckConfig
	logger      *zap.Logger
	balancer    LoadBalancer
	client      *http.Client
	stopCh      chan struct{}
	wg          sync.WaitGroup
	mu          sync.RWMutex
	status      map[string]HealthStatus
}

// HealthStatus represents the health status of a target.
type HealthStatus struct {
	TargetURL      string        `json:"target_url"`
	Healthy        bool          `json:"healthy"`
	LastCheck      time.Time     `json:"last_check"`
	LastSuccess    time.Time     `json:"last_success"`
	LastFailure    time.Time     `json:"last_failure,omitempty"`
	ConsecutiveOK  int           `json:"consecutive_ok"`
	ConsecutiveFail int         `json:"consecutive_fail"`
	Latency        time.Duration `json:"latency_ms"`
	Error          string        `json:"error,omitempty"`
}

// NewHealthChecker creates a new health checker for an upstream.
func NewHealthChecker(upstream string, targets []*Target, cfg config.HealthCheckConfig, logger *zap.Logger, balancer LoadBalancer) *HealthChecker {
	hc := &HealthChecker{
		upstream: upstream,
		targets:  targets,
		config:   cfg,
		logger:   logger.Named("healthcheck").With(zap.String("upstream", upstream)),
		balancer: balancer,
		client: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				DisableKeepAlives: true, // Fresh connection for each health check
			},
		},
		stopCh: make(chan struct{}),
		status: make(map[string]HealthStatus),
	}

	return hc
}

// Start begins the active health check loop.
func (hc *HealthChecker) Start() {
	interval := hc.config.Interval
	if interval <= 0 {
		interval = 10 * time.Second // Default interval
	}

	hc.wg.Add(1)
	go func() {
		defer hc.wg.Done()

		// Perform initial health check immediately
		hc.checkAll()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				hc.checkAll()
			case <-hc.stopCh:
				hc.logger.Debug("health checker stopped")
				return
			}
		}
	}()

	hc.logger.Info("health checker started",
		zap.String("interval", interval.String()),
		zap.String("path", hc.config.Path),
		zap.Int("targets", len(hc.targets)),
	)
}

// Stop halts the health check loop.
func (hc *HealthChecker) Stop() {
	close(hc.stopCh)
	hc.wg.Wait()
}

// checkAll performs health checks on all targets.
func (hc *HealthChecker) checkAll() {
	for _, target := range hc.targets {
		hc.wg.Add(1)
		go func(t *Target) {
			defer hc.wg.Done()
			hc.checkTarget(t)
		}(target)
	}
}

// checkTarget performs a single health check on a target.
func (hc *HealthChecker) checkTarget(target *Target) {
	start := time.Now()

	// Build health check URL
	checkURL := *target.URL
	checkURL.Path = hc.config.Path

	ctx, cancel := context.WithTimeout(context.Background(), hc.config.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL.String(), nil)
	if err != nil {
		hc.recordFailure(target, err.Error())
		return
	}

	resp, err := hc.client.Do(req)
	latency := time.Since(start)

	if err != nil {
		hc.recordFailure(target, err.Error())
		return
	}
	defer resp.Body.Close()

	// Check if response status indicates health
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		hc.recordSuccess(target, latency)
	} else {
		hc.recordFailure(target, "non-2xx status code: "+resp.Status)
	}
}

// recordSuccess records a successful health check.
func (hc *HealthChecker) recordSuccess(target *Target, latency time.Duration) {
	target.mu.Lock()
	wasHealthy := target.Healthy
	target.Healthy = true
	target.FailCount = 0
	target.mu.Unlock()

	hc.mu.Lock()
	status := hc.status[target.URL.String()]
	status.TargetURL = target.URL.String()
	status.Healthy = true
	status.LastCheck = time.Now()
	status.LastSuccess = time.Now()
	status.ConsecutiveOK++
	status.ConsecutiveFail = 0
	status.Latency = latency
	status.Error = ""
	hc.status[target.URL.String()] = status
	hc.mu.Unlock()

	// Notify balancer
	if !wasHealthy {
		hc.balancer.MarkHealthy(target.URL.String(), true)
		middleware.RegisterUpstreamHealth(hc.upstream, target.URL.String(), true)
		hc.logger.Info("target became healthy",
			zap.String("target", target.URL.String()),
			zap.Duration("latency", latency),
		)
	}

	hc.logger.Debug("health check success",
		zap.String("target", target.URL.String()),
		zap.Duration("latency", latency),
		zap.Int("consecutive_ok", status.ConsecutiveOK),
	)
}

// recordFailure records a failed health check.
func (hc *HealthChecker) recordFailure(target *Target, errMsg string) {
	target.mu.Lock()
	wasHealthy := target.Healthy
	target.FailCount++
	failures := target.FailCount
	// Mark unhealthy if failure threshold exceeded
	if failures >= int64(hc.config.Failures) {
		target.Healthy = false
	}
	target.mu.Unlock()

	hc.mu.Lock()
	status := hc.status[target.URL.String()]
	status.TargetURL = target.URL.String()
	status.LastCheck = time.Now()
	status.LastFailure = time.Now()
	status.ConsecutiveOK = 0
	status.ConsecutiveFail++
	status.Error = errMsg

	if failures >= int64(hc.config.Failures) {
		status.Healthy = false
	}
	hc.status[target.URL.String()] = status
	hc.mu.Unlock()

	// Notify balancer if target became unhealthy
	if wasHealthy && failures >= int64(hc.config.Failures) {
		hc.balancer.MarkHealthy(target.URL.String(), false)
		middleware.RegisterUpstreamHealth(hc.upstream, target.URL.String(), false)
		hc.logger.Warn("target marked unhealthy",
			zap.String("target", target.URL.String()),
			zap.Int64("failures", failures),
			zap.String("error", errMsg),
		)
	}

	hc.logger.Debug("health check failure",
		zap.String("target", target.URL.String()),
		zap.String("error", errMsg),
		zap.Int64("fail_count", failures),
	)
}

// GetStatus returns the current health status of all targets.
func (hc *HealthChecker) GetStatus() map[string]HealthStatus {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	result := make(map[string]HealthStatus, len(hc.status))
	for k, v := range hc.status {
		result[k] = v
	}
	return result
}

// IsHealthy returns true if the target is currently healthy.
func (hc *HealthChecker) IsHealthy(targetURL string) bool {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	if status, ok := hc.status[targetURL]; ok {
		return status.Healthy
	}
	return true // Default to healthy if no check performed yet
}

// PassiveHealthCheck performs a passive health check based on request results.
// This is called from the proxy layer after requests complete.
func PassiveHealthCheck(target *Target, statusCode int, err error) {
	if err != nil {
		target.mu.Lock()
		target.FailCount++
		target.mu.Unlock()
		return
	}

	// Reset failure count on success
	if statusCode >= 200 && statusCode < 500 {
		target.mu.Lock()
		target.FailCount = 0
		target.mu.Unlock()
	} else {
		// Server errors increment failure count
		if statusCode >= 500 {
			target.mu.Lock()
			target.FailCount++
			target.mu.Unlock()
		}
	}
}
