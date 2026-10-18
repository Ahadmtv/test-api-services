package auth

import (
	"testing"
	"time"
)

func TestRateLimiterAllowsBurst(t *testing.T) {
	rl := NewRateLimiter(60) // 1 per second, burst 60
	for i := 0; i < 60; i++ {
		if !rl.Allow("ip1", "ep") {
			t.Errorf("request %d should be allowed (burst)", i)
		}
	}
	if rl.Allow("ip1", "ep") {
		t.Error("request beyond burst should be rejected")
	}
}

func TestRateLimiterReplenishes(t *testing.T) {
	rl := NewRateLimiter(60) // 1/sec
	for i := 0; i < 60; i++ {
		rl.Allow("ip1", "ep")
	}
	if rl.Allow("ip1", "ep") {
		t.Error("should be exhausted")
	}
	time.Sleep(2 * time.Second)
	if !rl.Allow("ip1", "ep") {
		t.Error("should be allowed after replenish")
	}
}

func TestRateLimiterKeyedByEndpoint(t *testing.T) {
	rl := NewRateLimiter(1)
	if !rl.Allow("ip1", "epa") {
		t.Error("epa first request allowed")
	}
	if rl.Allow("ip1", "epa") {
		t.Error("epa second rejected")
	}
	// Different endpoint is independent.
	if !rl.Allow("ip1", "epb") {
		t.Error("epb first request allowed")
	}
	// Different IP independent.
	if !rl.Allow("ip2", "epa") {
		t.Error("epa for ip2 allowed")
	}
}
