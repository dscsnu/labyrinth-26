package httpx

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a token bucket per key (a user): each key may make PerSecond
// requests a second on average, in bursts of up to Burst. Buckets idle for
// longer than Idle are dropped (a fresh bucket is full, so dropping one
// never limits anyone more). Safe for concurrent use.
type Limiter struct {
	perSecond rate.Limit
	burst     int
	idle      time.Duration
	now       func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

// bucket is one key's token bucket and when it was last used.
type bucket struct {
	lim  *rate.Limiter
	used time.Time
}

// NewLimiter returns a limiter of perSecond requests a second per key, in
// bursts of up to burst, forgetting keys idle for longer than idle. now is
// its clock; nil means time.Now (tests pass a fake).
func NewLimiter(perSecond float64, burst int, idle time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{perSecond: rate.Limit(perSecond), burst: burst, idle: idle, now: now, buckets: map[string]*bucket{}}
}

// Allow takes a token from key's bucket. If there's none it returns false
// and how long until there will be one.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > l.idle { // at most once per idle period: O(keys)
		for k, b := range l.buckets {
			if now.Sub(b.used) > l.idle {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{lim: rate.NewLimiter(l.perSecond, l.burst)}
		l.buckets[key] = b
	}
	b.used = now
	r := b.lim.ReserveN(now, 1)
	if delay := r.DelayFrom(now); delay > 0 {
		r.CancelAt(now) // not taken: the request is refused
		return false, delay
	}
	return true, 0
}

// Len returns how many keys have a bucket (for tests and monitoring).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Middleware limits next by key(r): over the limit, it answers 429
// rate_limited with Retry-After (whole seconds, at least 1) and doesn't call
// next. key must identify the caller, so it runs after authentication.
func (l *Limiter) Middleware(key func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.Allow(key(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
			ErrorCode(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down and retry")
			return
		}
		next.ServeHTTP(w, r)
	})
}
