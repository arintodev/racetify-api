package auth

import (
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/httpapi/middleware"
	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/originpolicy"
	"github.com/racetify/racetify-api/internal/security"
)

// Handler is the thin HTTP adapter for this bounded context: decode
// request, call Service/GoogleService, encode response. It holds only
// *Service (not also a raw *Repository, unlike the pre-Step-8
// AuthHandler) - Me now calls the newly added Service.GetUserByID, since a
// bounded context's handler should only ever need its own service.
type Handler struct {
	auth   *Service
	google *GoogleService
	cfg    config.AuthConfig
	// origins decides which frontends Google sign-in may return the
	// browser to.
	origins *originpolicy.Policy
}

func NewHandler(auth *Service, google *GoogleService, cfg config.AuthConfig, origins *originpolicy.Policy) *Handler {
	return &Handler{auth: auth, google: google, cfg: cfg, origins: origins}
}

type registerRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// Register handles POST /api/v1/auth/register - Runner self-registration.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || len(req.Password) < 8 || strings.TrimSpace(req.FirstName) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "email, a password of at least 8 characters, and first_name are required.")
		return
	}

	user, err := h.auth.Register(r.Context(), req.Email, req.Password, req.FirstName, req.LastName)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, userResponse(user))
}

type verifyEmailRequest struct {
	Token string `json:"token"`
}

// VerifyEmail handles POST /api/v1/auth/verify-email.
func (h *Handler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req verifyEmailRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if req.Token == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "token is required.")
		return
	}
	if err := h.auth.VerifyEmail(r.Context(), req.Token); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"verified": true})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Login handles POST /api/v1/auth/login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	user, tokens, err := h.auth.Login(r.Context(), req.Email, req.Password, clientInfo(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	h.respondSession(w, r, http.StatusOK, user, tokens)
}

// bearerToken extracts a raw JWT from the Authorization header, exactly
// like middleware.bearerToken - duplicated here (a handful of lines)
// rather than exported cross-package, matching this codebase's existing
// precedent for small, dependency-free string helpers (see
// normalizeEmail's doc comment for the same call).
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix)), true
}

// Refresh handles POST /api/v1/auth/refresh, rotating the refresh token
// carried in the HTTP-only cookie. Authentication for this endpoint is
// the refresh token alone - an access token is entirely optional, and when
// present (as a Bearer header, or in the access cookie a browser always
// sends) it does NOT have to be valid/unexpired (unlike everywhere else in
// this package): it's read only as a hint for which tenant to re-embed in
// the new access token, see Service.RefreshAccessToken's doc comment. A
// client that never sends one simply gets a tenant-less token back.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	rawRefresh, ok := readRefreshToken(w, r)
	if !ok {
		return
	}
	if rawRefresh == "" {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "No refresh session found.")
		return
	}
	previousAccessToken, ok := bearerToken(r)
	if !ok {
		if c, err := r.Cookie(AccessCookieName); err == nil {
			previousAccessToken = c.Value
		}
	}
	user, tokens, err := h.auth.RefreshAccessToken(r.Context(), rawRefresh, previousAccessToken, clientInfo(r))
	if err != nil {
		if !bodyDelivery(r) {
			h.clearSessionCookies(w)
		}
		respond.FromServiceError(w, err)
		return
	}
	h.respondSession(w, r, http.StatusOK, user, tokens)
}

type switchTenantRequest struct {
	TenantID string `json:"tenant_id"`
}

// SwitchTenant handles POST /api/v1/auth/switch-tenant: mints a new
// access token scoped to a tenant the caller is an active member of. It
// changes no server-side session state (see Service.SelectTenant's doc
// comment) and does not touch the refresh token; a browser gets the new
// access token in its cookie. Requires only
// RequireUserAuth, not RequireTenantForUser - selecting a tenant must
// work starting from a tenant-less token too (e.g. right after Login
// without a tenant_id), and switching from one tenant to another doesn't
// need the *current* tenant to still check out.
func (h *Handler) SwitchTenant(w http.ResponseWriter, r *http.Request) {
	var req switchTenantRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.TenantID) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "tenant_id is required.")
		return
	}

	userID, _ := reqctx.UserID(r.Context())
	isSuperAdmin := reqctx.IsSuperAdmin(r.Context())

	tokens, err := h.auth.SelectTenant(r.Context(), userID, isSuperAdmin, req.TenantID)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	if !bodyDelivery(r) {
		user, err := h.auth.GetUserByID(r.Context(), userID)
		if err != nil {
			respond.FromServiceError(w, err)
			return
		}
		h.setAccessCookies(w, user, tokens, h.sessionEnd(r))
	}
	respond.JSON(w, http.StatusOK, tenantAccessTokenResponse(tokens, bodyDelivery(r)))
}

// sessionEnd is when the browser session's cookies should expire, taken from
// the hint cookie the API itself set at sign-in (the refresh token's own
// expiry is not known to an endpoint that never reads it). A missing or
// tampered hint only affects that browser's cookie lifetime, so it falls back
// to the idle window.
func (h *Handler) sessionEnd(r *http.Request) time.Time {
	if c, err := r.Cookie(sessionHintCookieName); err == nil {
		if hint, ok := decodeSessionHint(c.Value); ok && hint.RefreshExp > time.Now().Unix() {
			return time.Unix(hint.RefreshExp, 0)
		}
	}
	return time.Now().Add(h.cfg.RefreshTokenTTL)
}

// Logout handles POST /api/v1/auth/logout. Requires RequireUserAuth so the
// access token's jti (for blacklisting) is available.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	userID, _ := reqctx.UserID(r.Context())
	jti, _ := reqctx.AccessTokenJTI(r.Context())
	expUnix, _ := reqctx.AccessTokenExpiresAtUnix(r.Context())

	refreshRaw, ok := readRefreshToken(w, r)
	if !ok {
		return
	}

	if err := h.auth.Logout(r.Context(), refreshRaw, jti, time.Unix(expUnix, 0), userID); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	if !bodyDelivery(r) {
		h.clearSessionCookies(w)
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"logged_out": true})
}

// Me handles GET /api/v1/users/me.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	userID, _ := reqctx.UserID(r.Context())
	user, err := h.auth.GetUserByID(r.Context(), userID)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, userResponse(user))
}

type signupStartRequest struct {
	Email string `json:"email"`
}

// SignupStart handles POST /api/v1/auth/signup/start (and its alias
// /signup/resend): sends a verification OTP to the email. The response is
// identical whether or not the address already has an account.
func (h *Handler) SignupStart(w http.ResponseWriter, r *http.Request) {
	var req signupStartRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if !looksLikeEmail(req.Email) {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "A valid email is required.")
		return
	}
	if err := h.auth.StartSignup(r.Context(), req.Email); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	otpTTL, cooldown := h.auth.SignupTiming()
	respond.JSON(w, http.StatusAccepted, map[string]int{
		"expires_in_seconds":   int(otpTTL.Seconds()),
		"resend_after_seconds": int(cooldown.Seconds()),
	})
}

type signupVerifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// SignupVerify handles POST /api/v1/auth/signup/verify.
func (h *Handler) SignupVerify(w http.ResponseWriter, r *http.Request) {
	var req signupVerifyRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if !looksLikeEmail(req.Email) || strings.TrimSpace(req.Code) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "email and code are required.")
		return
	}
	token, err := h.auth.VerifySignupOTP(r.Context(), req.Email, req.Code)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]string{"signup_token": token})
}

type signupCompleteRequest struct {
	SignupToken   string `json:"signup_token"`
	FirstName     string `json:"first_name"`
	LastName      string `json:"last_name"`
	Password      string `json:"password"`
	AcceptedTerms bool   `json:"accepted_terms"`
}

// SignupComplete handles POST /api/v1/auth/signup/complete: creates the
// verified account and signs the user in.
func (h *Handler) SignupComplete(w http.ResponseWriter, r *http.Request) {
	var req signupCompleteRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if req.SignupToken == "" || len(req.Password) < 8 || strings.TrimSpace(req.FirstName) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "signup_token, first_name, and a password of at least 8 characters are required.")
		return
	}
	user, tokens, err := h.auth.CompleteSignup(r.Context(), req.SignupToken, req.FirstName, req.LastName, req.Password, req.AcceptedTerms, clientInfo(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	h.respondSession(w, r, http.StatusCreated, user, tokens)
}

// GoogleLogin handles GET /api/v1/auth/google/login?return_to=<url> by
// redirecting the browser into Google's consent screen. return_to is where
// the callback sends the browser afterwards; it must sit on an allowed
// frontend origin, or this would be an open redirect.
func (h *Handler) GoogleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.google.Enabled() {
		respond.Error(w, http.StatusNotImplemented, "not_configured", "Google sign-in is not configured on this environment.")
		return
	}
	returnTo := r.URL.Query().Get("return_to")
	if !h.returnToAllowed(returnTo) {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "return_to must be a URL on an allowed frontend origin.")
		return
	}
	// Binds this attempt to this browser: the callback only proceeds for the
	// browser that holds the nonce, so a callback URL handed to someone else
	// cannot sign them into the attacker's account (login CSRF).
	nonce, err := security.GenerateOpaqueToken(24)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	url, err := h.google.BeginAuthorization(r.Context(), returnTo, nonce)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	h.setCookie(w, cookieOpts{name: googleNonceCookieName, value: nonce, path: googleCookiePath, expires: time.Now().Add(10 * time.Minute), httpOnly: true, hostOnly: true})
	http.Redirect(w, r, url, http.StatusFound)
}

// GoogleCallback handles GET /api/v1/auth/google/callback. It never
// returns a session: it redirects back to the originating frontend with a
// one-time exchange code (see google_flow.go for why).
func (h *Handler) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "Missing code or state.")
		return
	}

	nonce := ""
	if c, err := r.Cookie(googleNonceCookieName); err == nil {
		nonce = c.Value
	}
	h.setCookie(w, cookieOpts{name: googleNonceCookieName, path: googleCookiePath, httpOnly: true, hostOnly: true})

	info, returnTo, err := h.google.CompleteAuthorization(r.Context(), code, state, nonce)
	if err != nil {
		if h.returnToAllowed(returnTo) {
			redirectWithParams(w, r, returnTo, map[string]string{"error": "google_failed"})
			return
		}
		respond.FromServiceError(w, err)
		return
	}
	if !h.returnToAllowed(returnTo) {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "Invalid return_to.")
		return
	}

	exchangeCode, err := h.auth.ResolveGoogleIdentity(r.Context(), info)
	if err != nil {
		redirectWithParams(w, r, returnTo, map[string]string{"error": "google_failed"})
		return
	}
	redirectWithParams(w, r, returnTo, map[string]string{"code": exchangeCode})
}

type googleExchangeRequest struct {
	Code string `json:"code"`
}

// googleExchangeResponse is either a session (embedded) or, for a Google
// identity with no account yet, needs_profile plus what the consent step
// needs.
type googleExchangeResponse struct {
	*SessionDTO
	NeedsProfile bool           `json:"needs_profile,omitempty"`
	PendingToken string         `json:"pending_token,omitempty"`
	Profile      *googleProfile `json:"profile,omitempty"`
}

type googleProfile struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// GoogleExchange handles POST /api/v1/auth/google/exchange.
func (h *Handler) GoogleExchange(w http.ResponseWriter, r *http.Request) {
	var req googleExchangeRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if req.Code == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "code is required.")
		return
	}
	res, err := h.auth.ExchangeGoogleCode(r.Context(), req.Code, clientInfo(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	if res.Tokens == nil {
		resp := googleExchangeResponse{
			NeedsProfile: true,
			Profile: &googleProfile{
				Email: res.Profile.Email, FirstName: res.Profile.FirstName, LastName: res.Profile.LastName,
			},
		}
		if bodyDelivery(r) {
			resp.PendingToken = res.PendingToken
		} else {
			// The pending token lets its holder create the account, so a
			// browser keeps it out of reach of scripts.
			h.setCookie(w, cookieOpts{name: googlePendingCookieName, value: res.PendingToken, path: googleCookiePath, expires: time.Now().Add(googlePendingTTL), httpOnly: true, hostOnly: true})
		}
		respond.JSON(w, http.StatusOK, resp)
		return
	}
	dto := sessionResponse(res.User, res.Tokens, bodyDelivery(r))
	if !bodyDelivery(r) {
		h.setSessionCookies(w, res.User, res.Tokens)
	}
	respond.JSON(w, http.StatusOK, googleExchangeResponse{SessionDTO: &dto})
}

type googleCompleteRequest struct {
	PendingToken  string `json:"pending_token"`
	FirstName     string `json:"first_name"`
	LastName      string `json:"last_name"`
	AcceptedTerms bool   `json:"accepted_terms"`
}

// GoogleComplete handles POST /api/v1/auth/google/complete.
func (h *Handler) GoogleComplete(w http.ResponseWriter, r *http.Request) {
	var req googleCompleteRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	pending := req.PendingToken
	if pending == "" {
		if c, err := r.Cookie(googlePendingCookieName); err == nil {
			pending = c.Value
		}
	}
	if pending == "" || strings.TrimSpace(req.FirstName) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "A pending Google sign-up and first_name are required.")
		return
	}
	user, tokens, err := h.auth.CompleteGoogleSignup(r.Context(), pending, req.FirstName, req.LastName, req.AcceptedTerms, clientInfo(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	h.setCookie(w, cookieOpts{name: googlePendingCookieName, path: googleCookiePath, httpOnly: true, hostOnly: true})
	h.respondSession(w, r, http.StatusCreated, user, tokens)
}

// ---- request helpers ----

// bodyDelivery reports whether the caller is a non-browser client (a mobile
// app, a service) that wants the tokens in the JSON body and manages them
// itself, instead of the browser-style HttpOnly cookies. Opting in grants
// nothing extra: the same credentials were required to obtain the session
// either way.
func bodyDelivery(r *http.Request) bool {
	return r.Header.Get("X-Token-Delivery") == "body"
}

// clientInfo extracts what is recorded about the caller of a session
// operation: user agent, IP, and the frontend host the session was started
// from (the Origin header a browser sends on every cross-origin call).
func clientInfo(r *http.Request) ClientInfo {
	info := ClientInfo{
		UserAgent:  r.UserAgent(),
		IP:         middleware.ClientIP(r),
		OriginHost: r.Header.Get("X-Session-Origin"),
	}
	if info.OriginHost == "" {
		if u, err := neturl.Parse(r.Header.Get("Origin")); err == nil {
			info.OriginHost = u.Host
		}
	}
	return info
}

type refreshTokenBody struct {
	RefreshToken string `json:"refresh_token"`
}

// readRefreshToken returns the refresh token from the JSON body (body
// delivery) or the cookie (browser delivery). ok is false when a response
// has already been written.
func readRefreshToken(w http.ResponseWriter, r *http.Request) (token string, ok bool) {
	if bodyDelivery(r) {
		var body refreshTokenBody
		if !respond.DecodeJSON(w, r, &body) {
			return "", false
		}
		return body.RefreshToken, true
	}
	if cookie, err := r.Cookie(refreshCookieName); err == nil {
		return cookie.Value, true
	}
	return "", true
}

// respondSession writes a session response, delivering the tokens per the
// caller's chosen mode: in the body, or in the browser's cookies.
func (h *Handler) respondSession(w http.ResponseWriter, r *http.Request, status int, user *User, tokens *SessionTokens) {
	if bodyDelivery(r) {
		respond.JSON(w, status, sessionResponse(user, tokens, true))
		return
	}
	h.setSessionCookies(w, user, tokens)
	respond.JSON(w, status, sessionResponse(user, tokens, false))
}

func looksLikeEmail(s string) bool {
	s = strings.TrimSpace(s)
	at := strings.Index(s, "@")
	return at > 0 && at < len(s)-1 && strings.Contains(s[at:], ".") && len(s) <= 254 && !strings.ContainsAny(s, " \t\r\n")
}

// returnToAllowed accepts only an absolute http(s) URL whose origin the
// frontend policy allows (exact origins and wildcard subdomains).
func (h *Handler) returnToAllowed(raw string) bool {
	return raw != "" && h.origins.Allowed(raw)
}

func redirectWithParams(w http.ResponseWriter, r *http.Request, to string, params map[string]string) {
	u, err := neturl.Parse(to)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "Invalid return_to.")
		return
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
