package auth

import (
	"sync"
	"time"
)

// LoginWindow is the sliding window of the per-IP login rate limit.
const LoginWindow = 15 * time.Minute

// RateLimiter is an in-memory sliding-window limiter keyed by client IP.
// In-memory state is fine because the app runs as a single replica.
type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
	now    func() time.Time
}

// NewRateLimiter allows limit events per key within window.
func NewRateLimiter(limit int, window time.Duration, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}, now: now}
}

func (r *RateLimiter) prune(key string, now time.Time) []time.Time {
	cutoff := now.Add(-r.window)
	h := r.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cutoff) {
		i++
	}
	h = h[i:]
	if len(h) == 0 {
		delete(r.hits, key)
		return nil
	}
	r.hits[key] = h
	return h
}

// Allow records an attempt for key and reports whether it is permitted. When
// it is not, retryAfter tells when the oldest attempt leaves the window.
// Rejected attempts are not recorded, so waiting is enough to recover.
func (r *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	h := r.prune(key, now)
	if len(h) >= r.limit {
		return false, h[0].Add(r.window).Sub(now)
	}
	r.hits[key] = append(h, now)
	return true, 0
}

// Sweep drops expired entries for every key; call it periodically.
func (r *RateLimiter) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for k := range r.hits {
		r.prune(k, now)
	}
}
