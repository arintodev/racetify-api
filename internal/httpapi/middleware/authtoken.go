package middleware

import "net/http"

// AccessCookieName is the HttpOnly cookie a browser's access token travels
// in (set by the auth handlers, see internal/auth/cookies.go).
const AccessCookieName = "racetify_access"

// RefreshCookieName is the cookie holding a browser's refresh token.
const RefreshCookieName = "racetify_refresh_token"

// userAccessToken finds the caller's access token: an Authorization header
// (API clients) wins, otherwise the access cookie (browsers).
func userAccessToken(r *http.Request) (raw string, ok bool) {
	if raw, ok := bearerToken(r); ok {
		return raw, true
	}
	if c, err := r.Cookie(AccessCookieName); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}
