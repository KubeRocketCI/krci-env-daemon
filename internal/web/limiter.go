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
	now     func() time.Time
	buckets map[string]*rate.Limiter
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()

	b, ok := l.buckets[key]
	if !ok {
		b = rate.NewLimiter(statusRate, statusBurst)
		l.buckets[key] = b
	}

	l.mu.Unlock()

	return b.AllowN(l.now(), 1)
}
