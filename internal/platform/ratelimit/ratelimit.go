// Package ratelimit implements a Redis-backed fixed-window counter, used
// for the implementation guide's "Rate Limiting S2S" (per client_id) and
// "Brute Force Protection" (per IP, on /api/v1/auth/login and
// /oauth/token) requirements. It is a pure data-access-layer concern with
// zero HTTP/domain awareness (only internal/platform/rediscli), used by
// both the login-rate-limit middleware (internal/httpapi/middleware/
// ratelimit.go, wired in internal/httpapi/router.go) and internal/
// oauthclient's Service (token grant) -
// see internal/httpapi/middleware/ratelimit.go for the HTTP-layer wiring.
package ratelimit

import (
	"context"
	"time"

	"github.com/racetify/racetify-api/internal/platform/rediscli"
)

// Limiter is a Redis-backed fixed-window rate limiter.
type Limiter struct {
	redis *rediscli.Client
}

func NewLimiter(redis *rediscli.Client) *Limiter {
	return &Limiter{redis: redis}
}

// Allow increments the counter for key and reports whether the caller is
// still within limit for the current window. The window starts on the
// first request that creates the key (fixed window, not sliding - simple
// and sufficient for Phase 0; a token-bucket/sliding-log implementation
// can replace this later without touching call sites).
func (l *Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	count, err := l.redis.Incr(ctx, key)
	if err != nil {
		// Fail open: an unavailable rate limiter should not take down
		// every login/token request. Availability > strict throttling for
		// a Phase 0 baseline; revisit if abuse in production warrants
		// fail-closed instead.
		return true, nil
	}
	if count == 1 {
		_ = l.redis.Expire(ctx, key, window)
	}
	return count <= int64(limit), nil
}
