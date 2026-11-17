package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	defaultRegisterPerMinute = 10
	defaultValidatePerMinute = 30
	rateLimitWindow          = time.Minute
)

// rateLimiter is an in-process fixed-window counter keyed by user. It bounds
// how much shared GitLab work one authenticated caller can trigger per minute;
// multiple instances are not coordinated, which matches the single-process
// deployment model.
type rateLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	buckets map[int64]rateBucket
}

type rateBucket struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration, now func() time.Time) *rateLimiter {
	if limit <= 0 || window <= 0 || now == nil {
		return nil
	}
	return &rateLimiter{limit: limit, window: window, now: now, buckets: map[int64]rateBucket{}}
}

// allow reports whether the caller may proceed. A denial returns the time
// remaining in the current window so the response can set Retry-After.
func (l *rateLimiter) allow(userID int64) (time.Duration, bool) {
	if l == nil {
		return 0, true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, ok := l.buckets[userID]
	if !ok || !now.Before(bucket.start.Add(l.window)) {
		bucket = rateBucket{start: now}
	}
	if bucket.count >= l.limit {
		l.buckets[userID] = bucket
		return bucket.start.Add(l.window).Sub(now), false
	}
	bucket.count++
	l.buckets[userID] = bucket
	if len(l.buckets) > 1024 {
		for key, candidate := range l.buckets {
			if !now.Before(candidate.start.Add(l.window)) {
				delete(l.buckets, key)
			}
		}
	}
	return 0, true
}

// rateLimited wraps one handler with its own per-user bucket. It runs after
// authentication and authorization, so an unauthenticated request keeps its
// 401 and never consumes a caller's quota.
func (s *Server) rateLimited(endpoint string, limiter *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, ok := principalFrom(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required", nil)
			return
		}
		// The static bootstrap bearer has no GitLab identity; ID 0 gives it a
		// dedicated bucket instead of sharing a numeric user's quota. Logs and
		// responses never include credentials.
		userID := value.user.ID
		if value.bootstrapBearer {
			userID = 0
		}
		retryAfter, allowed := limiter.allow(userID)
		if !allowed {
			seconds := int((retryAfter + time.Second - 1) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			s.logger.Warn("request rate limited", "endpoint", endpoint, "user_id", userID, "retry_after_s", seconds)
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests; retry later", nil)
			return
		}
		next(w, r)
	}
}
