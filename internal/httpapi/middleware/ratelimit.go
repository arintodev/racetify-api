package middleware

import (
	"net"
	"net/http"
	"time"

	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/ratelimit"
)

// ClientIP returns the request's best-guess source IP, honoring a
// single-hop X-Forwarded-For (typical for a load balancer/reverse proxy
// deployment) before falling back to RemoteAddr.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := indexByte(xff, ','); i >= 0 {
			return xff[:i]
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// RateLimit implements the guide's brute-force / S2S throttling
// requirements: bucket namespaces the counter (so /auth/login and
// /oauth/token don't share a budget), keyFunc picks what identifies the
// caller (IP for login, client_id for the M2M grant), and limit/window
// define the fixed window.
func RateLimit(limiter *ratelimit.Limiter, bucket string, limit int, window time.Duration, keyFunc func(r *http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := "ratelimit:" + bucket + ":" + keyFunc(r)
			allowed, err := limiter.Allow(r.Context(), key, limit, window)
			if err == nil && !allowed {
				respond.Error(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please try again later.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
