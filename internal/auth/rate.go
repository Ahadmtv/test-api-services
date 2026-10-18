package auth

import (
	"sync"
	"time"
)

// visitor holds rate-limit state for a single (ip, endpoint) pair.
type visitor struct {
	tokens   float64
	lastSeen time.Time
}

// RateLimiter implements a token-bucket rate limiter keyed by (ip, endpoint).
type RateLimiter struct {
	rate     float64 // tokens per second
	burst    float64
	visitors map[string]*visitor
	mu       sync.Mutex
}

// NewRateLimiter creates a rate limiter allowing `ratePerMinute` requests per
// minute per (ip, endpoint), bursting up to the same number.
func NewRateLimiter(ratePerMinute int) *RateLimiter {
	return &RateLimiter{
		rate:     float64(ratePerMinute) / 60.0,
		burst:    float64(ratePerMinute),
		visitors: make(map[string]*visitor),
	}
}

// Allow reports whether a request is permitted and consumes a token if so.
func (r *RateLimiter) Allow(ip, endpoint string) bool {
	key := ip + ":" + endpoint
	now := time.Now()

	r.mu.Lock()
	v, ok := r.visitors[key]
	if !ok {
		v = &visitor{tokens: r.burst, lastSeen: now}
		r.visitors[key] = v
	}

	elapsed := now.Sub(v.lastSeen).Seconds()
	v.lastSeen = now
	v.tokens += elapsed * r.rate
	if v.tokens > r.burst {
		v.tokens = r.burst
	}

	allowed := v.tokens >= 1.0
	if allowed {
		v.tokens--
	}
	r.mu.Unlock()

	// Periodic cleanup is intentionally omitted for stage 02; visitor map is
	// bounded by distinct (ip, endpoint) pairs observed in the process lifetime.
	return allowed
}
