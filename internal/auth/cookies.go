package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/httpapi/middleware"
)

// Browser sessions live in three cookies the API sets itself, so a frontend
// never proxies a login: the browser calls the API directly.
//
//	racetify_access         HttpOnly  the access JWT; read by middleware.RequireUserAuth
//	racetify_refresh_token  HttpOnly  the rotating refresh token; only sent to /api/v1/auth
//	racetify_session        readable  who is signed in (no secrets), so a frontend can
//	                                  render and gate pages without a round trip
//
// All three carry the configured Domain (the parent domain the API and the
// frontends share), so the browser treats them as first-party.
const (
	refreshCookieName     = "racetify_refresh_token"
	sessionHintCookieName = "racetify_session"

	// The Google sign-in helpers are only ever used on the API's own host, so
	// they stay host-only.
	googleNonceCookieName   = "racetify_gnonce"
	googlePendingCookieName = "racetify_gpending"

	apiPath           = "/api/v1"
	refreshCookiePath = apiPath + "/auth"
	googleCookiePath  = apiPath + "/auth/google"
)

// AccessCookieName is the cookie the auth middleware reads a browser's
// access token from.
const AccessCookieName = middleware.AccessCookieName

// sessionHint is the readable cookie's payload. It is a display and routing
// hint only: nothing trusts it, every request is authorized from the tokens.
type sessionHint struct {
	User UserDTO `json:"u"`
	// TenantID is the tenant the access token is scoped to, "" if none.
	TenantID string `json:"t,omitempty"`
	// AccessExp / RefreshExp are unix seconds. A client renews shortly before
	// AccessExp; RefreshExp is when the whole session ends.
	AccessExp  int64 `json:"ax"`
	RefreshExp int64 `json:"rx"`
}

func encodeSessionHint(h sessionHint) string {
	b, _ := json.Marshal(h)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeSessionHint(raw string) (sessionHint, bool) {
	var h sessionHint
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &h) != nil {
		return sessionHint{}, false
	}
	return h, true
}

type cookieOpts struct {
	cfg      config.AuthConfig
	name     string
	value    string
	path     string
	expires  time.Time
	httpOnly bool
	// hostOnly drops the Domain attribute.
	hostOnly bool
}

func (o cookieOpts) cookie() *http.Cookie {
	c := &http.Cookie{
		Name:     o.name,
		Value:    o.value,
		Path:     o.path,
		HttpOnly: o.httpOnly,
		Secure:   o.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	if !o.hostOnly {
		c.Domain = o.cfg.CookieDomain
	}
	if o.value == "" {
		c.MaxAge = -1
	} else if !o.expires.IsZero() {
		c.Expires = o.expires
	}
	return c
}

// setSessionCookies stores a freshly issued session in the browser.
func (h *Handler) setSessionCookies(w http.ResponseWriter, user *User, t *SessionTokens) {
	h.setCookie(w, cookieOpts{name: refreshCookieName, value: t.RefreshToken, path: refreshCookiePath, expires: t.RefreshTokenExpiresAt, httpOnly: true})
	h.setAccessCookies(w, user, t, t.RefreshTokenExpiresAt)
}

// setAccessCookies replaces the access token (and the hint that mirrors it)
// without touching the refresh token: used when only the tenant scope changes.
// The cookies live as long as the refresh token, not the access token, so a
// lapsed access token still reaches the API - which answers 401 and lets the
// client renew - instead of vanishing from the browser.
func (h *Handler) setAccessCookies(w http.ResponseWriter, user *User, t *SessionTokens, refreshExpiresAt time.Time) {
	h.setCookie(w, cookieOpts{name: AccessCookieName, value: t.AccessToken, path: apiPath, expires: refreshExpiresAt, httpOnly: true})
	h.setCookie(w, cookieOpts{
		name: sessionHintCookieName,
		value: encodeSessionHint(sessionHint{
			User:       userResponse(user),
			TenantID:   t.TenantID,
			AccessExp:  t.AccessTokenExpiresAt.Unix(),
			RefreshExp: refreshExpiresAt.Unix(),
		}),
		path:    "/",
		expires: refreshExpiresAt,
	})
}

func (h *Handler) clearSessionCookies(w http.ResponseWriter) {
	h.setCookie(w, cookieOpts{name: refreshCookieName, path: refreshCookiePath, httpOnly: true})
	h.setCookie(w, cookieOpts{name: AccessCookieName, path: apiPath, httpOnly: true})
	h.setCookie(w, cookieOpts{name: sessionHintCookieName, path: "/"})
}

func (h *Handler) setCookie(w http.ResponseWriter, o cookieOpts) {
	o.cfg = h.cfg
	http.SetCookie(w, o.cookie())
}
