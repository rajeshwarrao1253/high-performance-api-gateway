// Package upstream provides load balancing algorithms for distributing requests
// across multiple backend targets. It supports round-robin, least-connections,
// weighted, and random algorithms with health-aware selection.
package upstream

import (
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/middleware"
)

// Target represents a single upstream backend server.
type Target struct {
	URL        *url.URL
	Weight     int
	Priority   int
	Healthy    bool
	ActiveConns int64
	FailCount  int64
	mu         sync.RWMutex
}

// LoadBalancer distributes requests across multiple targets.
type LoadBalancer interface {
	// Next selects the next target for the given request.
	Next(r *http.Request) *Target
	// TrackRequest returns a done function to call when the request completes.
	TrackRequest(target *Target) func()
	// MarkHealthy marks a target as healthy or unhealthy.
	MarkHealthy(targetURL string, healthy bool)
	// Targets returns all targets managed by this balancer.
	Targets() []*Target
}

// NewLoadBalancer creates a load balancer with the given algorithm.
func NewLoadBalancer(algorithm string, targets []Target) LoadBalancer {
	// Filter to only healthy targets initially (all start healthy)
	targetPtrs := make([]*Target, len(targets))
	for i := range targets {
		t := &targets[i]
		if t.Weight <= 0 {
			t.Weight = 1 // Default weight
		}
		t.Healthy = true
		targetPtrs[i] = t
	}

	switch algorithm {
	case "least_connections":
		return &LeastConnectionsBalancer{targets: targetPtrs}
	case "weighted":
		return NewWeightedBalancer(targetPtrs)
	case "random":
		return &RandomBalancer{targets: targetPtrs}
	case "round_robin":
		fallthrough
	default:
		return &RoundRobinBalancer{targets: targetPtrs}
	}
}

// RoundRobinBalancer implements round-robin load balancing.
type RoundRobinBalancer struct {
	targets []*Target
	current uint64
}

// Next selects the next target using round-robin.
func (rb *RoundRobinBalancer) Next(r *http.Request) *Target {
	healthy := rb.healthyTargets()
	if len(healthy) == 0 {
		return nil
	}

	idx := atomic.AddUint64(&rb.current, 1) % uint64(len(healthy))
	return healthy[idx]
}

// TrackRequest is a no-op for round-robin.
func (rb *RoundRobinBalancer) TrackRequest(target *Target) func() {
	return func() {}
}

// MarkHealthy updates the health status of a target.
func (rb *RoundRobinBalancer) MarkHealthy(targetURL string, healthy bool) {
	for _, t := range rb.targets {
		if t.URL.String() == targetURL {
			t.mu.Lock()
			t.Healthy = healthy
			if healthy {
				t.FailCount = 0
			}
			t.mu.Unlock()
			break
		}
	}
}

// Targets returns all targets.
func (rb *RoundRobinBalancer) Targets() []*Target {
	return rb.targets
}

// healthyTargets returns only healthy targets.
func (rb *RoundRobinBalancer) healthyTargets() []*Target {
	var healthy []*Target
	for _, t := range rb.targets {
		t.mu.RLock()
		if t.Healthy {
			healthy = append(healthy, t)
		}
		t.mu.RUnlock()
	}
	return healthy
}

// LeastConnectionsBalancer implements least-connections load balancing.
type LeastConnectionsBalancer struct {
	targets []*Target
}

// Next selects the target with the fewest active connections.
func (lc *LeastConnectionsBalancer) Next(r *http.Request) *Target {
	healthy := lc.healthyTargets()
	if len(healthy) == 0 {
		return nil
	}

	// Find target with minimum active connections
	var selected *Target
	var minConns int64 = -1

	for _, t := range healthy {
		conns := atomic.LoadInt64(&t.ActiveConns)
		if minConns == -1 || conns < minConns {
			minConns = conns
			selected = t
		}
	}

	return selected
}

// TrackRequest increments/decrements active connection count.
func (lc *LeastConnectionsBalancer) TrackRequest(target *Target) func() {
	if target != nil {
		atomic.AddInt64(&target.ActiveConns, 1)
	}
	return func() {
		if target != nil {
			atomic.AddInt64(&target.ActiveConns, -1)
		}
	}
}

// MarkHealthy updates the health status of a target.
func (lc *LeastConnectionsBalancer) MarkHealthy(targetURL string, healthy bool) {
	for _, t := range lc.targets {
		if t.URL.String() == targetURL {
			t.mu.Lock()
			t.Healthy = healthy
			if healthy {
				t.FailCount = 0
			}
			t.mu.Unlock()
			break
		}
	}
}

// Targets returns all targets.
func (lc *LeastConnectionsBalancer) Targets() []*Target {
	return lc.targets
}

// healthyTargets returns only healthy targets.
func (lc *LeastConnectionsBalancer) healthyTargets() []*Target {
	var healthy []*Target
	for _, t := range lc.targets {
		t.mu.RLock()
		if t.Healthy {
			healthy = append(healthy, t)
		}
		t.mu.RUnlock()
	}
	return healthy
}

// WeightedBalancer implements weighted round-robin load balancing.
type WeightedBalancer struct {
	targets       []*Target
	weights       []int
	currentIndex  int
	currentWeight int
	mu            sync.Mutex
}

// NewWeightedBalancer creates a new weighted round-robin balancer.
func NewWeightedBalancer(targets []*Target) *WeightedBalancer {
	wb := &WeightedBalancer{
		targets: targets,
		weights: make([]int, len(targets)),
	}
	for i, t := range targets {
		wb.weights[i] = t.Weight
	}
	return wb
}

// Next selects the next target using weighted round-robin (smooth WRR algorithm).
func (wb *WeightedBalancer) Next(r *http.Request) *Target {
	healthy := wb.healthyTargets()
	if len(healthy) == 0 {
		return nil
	}

	wb.mu.Lock()
	defer wb.mu.Unlock()

	// Smooth weighted round-robin algorithm
	maxWeight := -1
	var selected *Target

	for _, t := range healthy {
		t.mu.RLock()
		weight := t.Weight
		t.mu.RUnlock()

		// Decrement current weight
		wb.currentWeight -= weight

		if wb.currentWeight < 0 {
			wb.currentWeight = maxWeightSum(healthy) - weight
			selected = t
			break
		}
	}

	if selected == nil {
		selected = healthy[0]
	}

	return selected
}

// TrackRequest is a no-op for weighted balancer.
func (wb *WeightedBalancer) TrackRequest(target *Target) func() {
	return func() {}
}

// MarkHealthy updates the health status of a target.
func (wb *WeightedBalancer) MarkHealthy(targetURL string, healthy bool) {
	for _, t := range wb.targets {
		if t.URL.String() == targetURL {
			t.mu.Lock()
			t.Healthy = healthy
			if healthy {
				t.FailCount = 0
			}
			t.mu.Unlock()
			break
		}
	}
}

// Targets returns all targets.
func (wb *WeightedBalancer) Targets() []*Target {
	return wb.targets
}

// healthyTargets returns only healthy targets.
func (wb *WeightedBalancer) healthyTargets() []*Target {
	var healthy []*Target
	for _, t := range wb.targets {
		t.mu.RLock()
		if t.Healthy {
			healthy = append(healthy, t)
		}
		t.mu.RUnlock()
	}
	return healthy
}

// RandomBalancer implements random load balancing.
type RandomBalancer struct {
	targets []*Target
	rng     *rand.Rand
	mu      sync.Mutex
}

// Next selects a random healthy target.
func (rb *RandomBalancer) Next(r *http.Request) *Target {
	healthy := rb.healthyTargets()
	if len(healthy) == 0 {
		return nil
	}

	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.rng == nil {
		rb.rng = rand.New(rand.NewSource(1))
	}

	idx := rb.rng.Intn(len(healthy))
	return healthy[idx]
}

// TrackRequest is a no-op for random balancer.
func (rb *RandomBalancer) TrackRequest(target *Target) func() {
	return func() {}
}

// MarkHealthy updates the health status of a target.
func (rb *RandomBalancer) MarkHealthy(targetURL string, healthy bool) {
	for _, t := range rb.targets {
		if t.URL.String() == targetURL {
			t.mu.Lock()
			t.Healthy = healthy
			if healthy {
				t.FailCount = 0
			}
			t.mu.Unlock()
			break
		}
	}
}

// Targets returns all targets.
func (rb *RandomBalancer) Targets() []*Target {
	return rb.targets
}

// healthyTargets returns only healthy targets.
func (rb *RandomBalancer) healthyTargets() []*Target {
	var healthy []*Target
	for _, t := range rb.targets {
		t.mu.RLock()
		if t.Healthy {
			healthy = append(healthy, t)
		}
		t.mu.RUnlock()
	}
	return healthy
}

// maxWeightSum calculates the maximum weight sum for the smooth WRR algorithm.
func maxWeightSum(targets []*Target) int {
	sum := 0
	for _, t := range targets {
		t.mu.RLock()
		sum += t.Weight
		t.mu.RUnlock()
	}
	return sum
}
