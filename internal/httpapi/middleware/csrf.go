package middleware

import (
	"net/http"
	"strings"

	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/originpolicy"
)

// CSRF protects the cookie-authenticated browser API. The session cookies
// are SameSite=Lax, which stops other sites but still sends them on a
// request from any sibling subdomain of the shared cookie domain, so an
// unsafe request that relies on them (or that is a sign-in itself) must come
// from an allowed frontend origin.
//
// Requests that carry their own Authorization header, and requests with no
// browser credentials at all (curl, servers), are unaffected: nothing rides
// along automatically for an attacker to abuse.
func CSRF(policy *originpolicy.Policy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethod(r.Method) || r.Header.Get("Authorization") != "" {
				next.ServeHTTP(w, r)
				return
			}
			hasCookies := hasCookie(r, AccessCookieName) || hasCookie(r, RefreshCookieName)
			isAuthEndpoint := strings.HasPrefix(r.URL.Path, "/api/v1/auth/")
			if !hasCookies && !isAuthEndpoint {
				next.ServeHTTP(w, r)
				return
			}

			origin := r.Header.Get("Origin")
			switch {
			case origin == "":
				// Browsers always send Origin on a cross-origin or non-GET
				// fetch. Its absence with cookies attached is not a browser
				// we can vouch for; without cookies (a plain server call to
				// an auth endpoint) there is nothing to protect.
				if hasCookies {
					forbidden(w)
					return
				}
			case !policy.Allowed(origin):
				forbidden(w)
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				forbidden(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func hasCookie(r *http.Request, name string) bool {
	_, err := r.Cookie(name)
	return err == nil
}

func forbidden(w http.ResponseWriter) {
	respond.Error(w, http.StatusForbidden, "forbidden", "Cross-origin request refused.")
}
