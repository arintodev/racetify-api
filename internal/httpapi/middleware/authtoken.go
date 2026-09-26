package middleware

import "net/http"

// AccessCookieName is the HttpOnly cookie a browser's access token travels
// in (set by the auth handlers, see internal/auth/cookies.go).
const AccessCookieName = "racetify_access"

// userAccessToken finds the caller's access token: an Authorization header
// (API clients) wins, otherwise the access cookie (browsers). The second
// result says which, since a cookie is sent by the browser on its own and so
// needs the CSRF check in CSRF.
func userAccessToken(r *http.Request) (raw string, fromCookie, ok bool) {
	if raw, ok := bearerToken(r); ok {
		return raw, false, true
	}
	if c, err := r.Cookie(AccessCookieName); err == nil && c.Value != "" {
		return c.Value, true, true
	}
	return "", false, false
}
