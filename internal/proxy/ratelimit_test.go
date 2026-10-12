package proxy

import (
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(10, 20)
	now := time.Now()
	allowed := 0
	for i := 0; i < 30; i++ {
		if l.allow("1.2.3.4", now) {
			allowed++
		}
	}
	if allowed != 20 {
		t.Fatalf("burst: allowed %d, want 20", allowed)
	}
	if !l.allow("5.6.7.8", now) {
		t.Fatal("other clients are independent")
	}
	if !l.allow("1.2.3.4", now.Add(200*time.Millisecond)) {
		t.Fatal("tokens refill over time")
	}
	if newRateLimiter(0, 0) != nil {
		t.Fatal("0 disables limiting")
	}
}
