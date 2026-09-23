package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/httpapi/middleware"
	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
)

const refreshCookieName = "racetify_refresh_token"

// Handler is the thin HTTP adapter for this bounded context: decode
// request, call Service/GoogleService, encode response. It holds only
// *Service (not also a raw *Repository, unlike the pre-Step-8
// AuthHandler) - Me now calls the newly added Service.GetUserByID, since a
// bounded context's handler should only ever need its own service.
type Handler struct {
	auth   *Service
	google *GoogleService
	cfg    config.AuthConfig
}

func NewHandler(auth *Service, google *GoogleService, cfg config.AuthConfig) *Handler {
	return &Handler{auth: auth, google: google, cfg: cfg}
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
	user, tokens, err := h.auth.Login(r.Context(), req.Email, req.Password, r.UserAgent(), middleware.ClientIP(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	setRefreshCookie(w, tokens.RefreshToken, tokens.RefreshTokenExpiresAt)
	respond.JSON(w, http.StatusOK, sessionResponse(user, tokens))
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
// the refresh cookie alone - an Authorization header is entirely
// optional, and when present it does NOT have to be valid/unexpired
// (unlike everywhere else in this package): it's read only as a hint for
// which tenant to re-embed in the new access token, see
// Service.RefreshAccessToken's doc comment. A client that never sends one
// simply gets a tenant-less token back, exactly as before.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookieName)
	if err != nil || cookie.Value == "" {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "No refresh session found.")
		return
	}
	previousAccessToken, _ := bearerToken(r)
	user, tokens, err := h.auth.RefreshAccessToken(r.Context(), cookie.Value, previousAccessToken, r.UserAgent(), middleware.ClientIP(r))
	if err != nil {
		clearRefreshCookie(w)
		respond.FromServiceError(w, err)
		return
	}
	setRefreshCookie(w, tokens.RefreshToken, tokens.RefreshTokenExpiresAt)
	respond.JSON(w, http.StatusOK, sessionResponse(user, tokens))
}

type switchTenantRequest struct {
	TenantID string `json:"tenant_id"`
}

// SwitchTenant handles POST /api/v1/auth/switch-tenant: mints a new
// access token scoped to a tenant the caller is an active member of. It
// changes no server-side session state (see Service.SelectTenant's doc
// comment) and does not touch the refresh-token cookie. Requires only
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
	respond.JSON(w, http.StatusOK, tenantAccessTokenResponse(tokens))
}

// Logout handles POST /api/v1/auth/logout. Requires RequireUserAuth so the
// access token's jti (for blacklisting) is available.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	userID, _ := reqctx.UserID(r.Context())
	jti, _ := reqctx.AccessTokenJTI(r.Context())
	expUnix, _ := reqctx.AccessTokenExpiresAtUnix(r.Context())

	var refreshRaw string
	if cookie, err := r.Cookie(refreshCookieName); err == nil {
		refreshRaw = cookie.Value
	}

	if err := h.auth.Logout(r.Context(), refreshRaw, jti, time.Unix(expUnix, 0), userID); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	clearRefreshCookie(w)
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

// GoogleLogin handles GET /api/v1/auth/google/login by redirecting the
// browser into Google's consent screen.
func (h *Handler) GoogleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.google.Enabled() {
		respond.Error(w, http.StatusNotImplemented, "not_configured", "Google sign-in is not configured on this environment.")
		return
	}
	url, err := h.google.BeginAuthorization(r.Context())
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// GoogleCallback handles GET /api/v1/auth/google/callback.
func (h *Handler) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "Missing code or state.")
		return
	}

	info, err := h.google.CompleteAuthorization(r.Context(), code, state)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}

	user, tokens, err := h.auth.LoginOrRegisterWithGoogle(r.Context(), info, r.UserAgent(), middleware.ClientIP(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	setRefreshCookie(w, tokens.RefreshToken, tokens.RefreshTokenExpiresAt)
	respond.JSON(w, http.StatusOK, sessionResponse(user, tokens))
}

// ---- shared response shaping + cookie helpers ----

func setRefreshCookie(w http.ResponseWriter, value string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    value,
		Path:     "/api/v1/auth",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     "/api/v1/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
