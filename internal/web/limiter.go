package web

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiter keeps a token bucket per key. Keys are environment namespaces, never client
// input, so the number of buckets is bounded by the number of environments.
type limiter struct {
	mu      sync.Mutex
	rate    rate.Limit
	burst   int
	now     func() time.Time
	buckets map[string]*rate.Limiter
}

func newLimiter(perSecond rate.Limit, burst int, now func() time.Time) *limiter {
	return &limiter{rate: perSecond, burst: burst, now: now, buckets: map[string]*rate.Limiter{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()

	b, ok := l.buckets[key]
	if !ok {
		b = rate.NewLimiter(l.rate, l.burst)
		l.buckets[key] = b
	}

	l.mu.Unlock()

	return b.AllowN(l.now(), 1)
}
