package oauthclient

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/platform/ratelimit"
)

// RegisterRoutes wires tenant-scoped OAuth client (S2S credential)
// management plus the client_credentials token grant itself. The grant
// endpoint is unauthenticated (the request body IS the credential) and
// carries its own per-client_id throttle inside Handler.Token, on top of
// the per-IP m.TokenRateLimit applied here. Building the Handler here
// (rather than in the composition root) keeps the router's own wiring
// down to "pass this context its already-built Service" - see
// docs/phase0-refactor-plan.md §7 for the self-registering-routes pattern
// this follows.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service, limiter *ratelimit.Limiter) {
	h := NewHandler(svc, limiter)

	mux.Handle("POST /oauth/token", routing.Chain(h.Token, mw.TokenRateLimit))

	mux.Handle("POST /api/v1/oauth-clients", routing.Chain(h.Create, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/oauth-clients", routing.Chain(h.List, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/oauth-clients/{id}", routing.Chain(h.Revoke, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
}
