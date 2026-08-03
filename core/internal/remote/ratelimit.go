package remote

import (
	"sync"
	"time"
)

// authRateBurst is how many pairing attempts one source address may make inside
// authRateWindow.
//
// Five is chosen to be indistinguishable from normal for a human (a mistyped
// link, a reload, a browser prefetch) and useless for guessing: the token is 256
// bits, so even an unlimited attacker is not getting in — the limiter exists to
// stop an unauthenticated caller burning CPU on SHA-256 and filling the audit
// log, not to defend the secret.
const authRateBurst = 5

// authRateWindow is the refill period for the burst above.
const authRateWindow = time.Minute

// maxRateLimitIPs bounds the limiter's own memory. Without it, an attacker
// spoofing source addresses on a LAN turns the defence into an allocator: one
// map entry per forged address. When the table is full the oldest entry is
// evicted, which at worst grants one extra attempt to whoever it belonged to.
const maxRateLimitIPs = 4096

type rateBucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a per-source token bucket for POST /api/v1/auth/exchange — the
// only unauthenticated route in the API.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
	now     func() time.Time
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{buckets: make(map[string]*rateBucket), now: now}
}

// allow reports whether this source may make another attempt, consuming a token
// when it may.
func (l *rateLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxRateLimitIPs {
			l.evictOldestLocked()
		}
		b = &rateBucket{tokens: authRateBurst, last: now}
		l.buckets[key] = b
	}
	// Continuous refill rather than a fixed window: a fixed window lets a caller
	// spend its whole allowance at the boundary and the next one immediately
	// after, which is twice the burst it was granted.
	elapsed := now.Sub(b.last)
	if elapsed > 0 {
		b.tokens += elapsed.Seconds() * (authRateBurst / authRateWindow.Seconds())
		if b.tokens > authRateBurst {
			b.tokens = authRateBurst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictOldestLocked drops the least recently used bucket. Caller holds l.mu.
func (l *rateLimiter) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = k, b.last
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}
