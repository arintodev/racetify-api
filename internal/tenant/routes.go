package tenant

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/middleware"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/oauthclient"
)

// RegisterRoutes wires tenant creation/listing, invitation acceptance (all
// user-session authenticated, no tenant context resolved yet), the
// tenant-scoped member/invitation management endpoints, the Platform
// Super Admin verification endpoint, and (folded in per
// docs/phase0-refactor-plan.md §6) the Phase 0 demo resource endpoint
// reachable under both a user session and an M2M token.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service) {
	h := NewHandler(svc)

	// ---- user-session authenticated, no tenant context ----
	mux.Handle("POST /api/v1/tenants", routing.Chain(h.Create, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/tenants/me", routing.Chain(h.ListMine, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/invitations/accept", routing.Chain(h.AcceptInvitation, mw.RequireUserAuth))

	// ---- user-session authenticated, tenant-scoped ----
	mux.Handle("GET /api/v1/members", routing.Chain(h.ListMembers, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/invitations", routing.Chain(h.InviteStaff, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/invitations", routing.Chain(h.ListInvitations, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/invitations/{id}", routing.Chain(h.RevokeInvitation, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))

	// ---- Platform Super Admin, cross-tenant ----
	mux.Handle("PATCH /api/v1/admin/tenants/{id}/status", routing.Chain(h.SetStatus, mw.RequireSuperAdmin, mw.RequireUserAuth))

	// ---- Phase 0 demo resource endpoint (folded in from the former
	// httpapi/routes_resource.go - see docs/phase0-refactor-plan.md §6) ----
	// Reachable by a user session.
	mux.Handle("GET /api/v1/tenant/summary", routing.Chain(h.Summary, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	// Reachable by an M2M token.
	mux.Handle("GET /api/v1/m2m/tenant/summary", routing.Chain(
		h.Summary,
		middleware.RequireScope(oauthclient.ScopeTenantRead),
		middleware.RequireTenantForM2M,
		mw.RequireM2MAuth,
	))
}
