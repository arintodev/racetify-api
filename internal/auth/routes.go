package auth

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/platform/originpolicy"
)

// RegisterRoutes wires user registration, session (login/refresh/logout),
// Google OAuth login, and GET /api/v1/users/me. The OAuth 2.0
// client-credentials token grant (/oauth/token) and tenant-scoped OAuth
// *client* management routes belong to internal/oauthclient instead - see
// that package's routes.go.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service, google *GoogleService, cfg config.AuthConfig, origins *originpolicy.Policy) {
	h := NewHandler(svc, google, cfg, origins)

	// ---- unauthenticated ----
	mux.Handle("POST /api/v1/auth/register", routing.Chain(h.Register))
	mux.Handle("POST /api/v1/auth/verify-email", routing.Chain(h.VerifyEmail))
	mux.Handle("POST /api/v1/auth/login", routing.Chain(h.Login, mw.LoginRateLimit))
	mux.Handle("POST /api/v1/auth/refresh", routing.Chain(h.Refresh))
	mux.Handle("POST /api/v1/auth/signup/start", routing.Chain(h.SignupStart, mw.SignupRateLimit))
	mux.Handle("POST /api/v1/auth/signup/resend", routing.Chain(h.SignupStart, mw.SignupRateLimit))
	mux.Handle("POST /api/v1/auth/signup/verify", routing.Chain(h.SignupVerify, mw.SignupRateLimit))
	mux.Handle("POST /api/v1/auth/signup/complete", routing.Chain(h.SignupComplete, mw.SignupRateLimit))
	mux.Handle("POST /api/v1/auth/google/exchange", routing.Chain(h.GoogleExchange, mw.SignupRateLimit))
	mux.Handle("POST /api/v1/auth/google/complete", routing.Chain(h.GoogleComplete, mw.SignupRateLimit))
	mux.Handle("GET /api/v1/auth/google/login", routing.Chain(h.GoogleLogin))
	mux.Handle("GET /api/v1/auth/google/callback", routing.Chain(h.GoogleCallback))

	// ---- user-session authenticated, no tenant context ----
	mux.Handle("POST /api/v1/auth/logout", routing.Chain(h.Logout, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/users/me", routing.Chain(h.Me, mw.RequireUserAuth))
	// Mints a tenant-scoped access token (Service.SelectTenant) - see
	// Handler.SwitchTenant's doc comment for why this only needs
	// RequireUserAuth, not RequireTenantForUser.
	mux.Handle("POST /api/v1/auth/switch-tenant", routing.Chain(h.SwitchTenant, mw.RequireUserAuth))
}
