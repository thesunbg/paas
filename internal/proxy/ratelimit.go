package proxy

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a per-client-IP token bucket.
type rateLimiter struct {
	rate, burst float64
	mu          sync.Mutex
	buckets     map[string]*bucket
	lastSweep   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rps, burst int) *rateLimiter {
	if rps <= 0 {
		return nil
	}
	if burst < rps {
		burst = rps * 2
	}
	return &rateLimiter{rate: float64(rps), burst: float64(burst), buckets: map[string]*bucket{}, lastSweep: time.Now()}
}

func (l *rateLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *rateLimiter) wrap(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "1")
			errorPage(w, http.StatusTooManyRequests, "Too many requests", "Slow down and try again in a moment.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
