package server

import (
	"sync"
	"time"
)

// limiter is a simple per-client token-bucket rate limiter (DoS: high-volume query
// flood). Each client IP gets `rate` tokens/sec with a burst of `rate`. Stale buckets
// are lazily reclaimed. Cheap and dependency-free.
type limiter struct {
	rate float64
	mu   sync.Mutex
	b    map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(ratePerSec int) *limiter {
	return &limiter{rate: float64(ratePerSec), b: make(map[string]*bucket)}
}

func (l *limiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bk := l.b[ip]
	if bk == nil {
		bk = &bucket{tokens: l.rate, last: now}
		l.b[ip] = bk
		// opportunistic cleanup to bound memory under an IP-spray attack.
		if len(l.b) > 100000 {
			for k, v := range l.b {
				if now.Sub(v.last) > time.Minute {
					delete(l.b, k)
				}
			}
		}
	}
	// refill.
	elapsed := now.Sub(bk.last).Seconds()
	bk.tokens += elapsed * l.rate
	if bk.tokens > l.rate {
		bk.tokens = l.rate
	}
	bk.last = now
	if bk.tokens >= 1 {
		bk.tokens--
		return true
	}
	return false
}
