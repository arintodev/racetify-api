// Package middleware holds every net/http middleware in the request
// pipeline: request id / logging / panic recovery / CORS (this file),
// authentication (auth.go), tenant resolution (tenant.go), RBAC (rbac.go),
// and rate limiting (ratelimit.go).
package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/platform/originpolicy"
	"github.com/racetify/racetify-api/internal/security"
)

// RequestID stamps every request with a short opaque id, propagated via
// context and echoed back as X-Request-ID, so a single log line can be
// grepped end to end.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id, _ = security.GenerateOpaqueToken(8)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(reqctx.WithRequestID(r.Context(), id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Logging emits one structured log line per request. It wraps every other
// middleware, so it reports the final status code after auth/tenant/RBAC
// checks too.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			requestID, _ := reqctx.RequestID(r.Context())
			logger.InfoContext(r.Context(), "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", requestID,
			)
		})
	}
}

// Recover converts a panic anywhere downstream into a 500 instead of
// crashing the process / hanging the connection.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.ErrorContext(r.Context(), "http handler panic", "panic", rec, "path", r.URL.Path)
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"Something went wrong. Please try again."}}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// CORS applies the guide's "CORS Configuration: diterapkan secara
// terisolasi" requirement: an explicit allowlist of origins rather than a
// blanket '*', with the OAuth S2S endpoint (mounted separately as it takes
// no browser cookies) allowed to accept requests from any registered
// server-side caller.
//
// Browsers call this API directly with credentials (cookies), so an allowed
// origin is echoed back with Allow-Credentials. The policy may hold wildcard
// entries (https://*.racetify.com) for tenant subdomains.
func CORS(policy *originpolicy.Policy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			w.Header().Add("Vary", "Origin")
			if origin != "" && policy.Allowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID, X-Token-Delivery")
			// Lets the browser reuse one preflight for a while instead of
			// paying an extra round trip before every JSON POST.
			w.Header().Set("Access-Control-Max-Age", "7200")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
