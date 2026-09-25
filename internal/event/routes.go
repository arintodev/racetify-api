package event

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/middleware"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/oauthclient"
	"github.com/racetify/racetify-api/internal/platform/originpolicy"
)

// RegisterRoutes wires the tenant-scoped Event/Race CRUD endpoints and
// their read-only M2M counterparts, per docs/phase1-api-plan.md §4.1.
// mw.RequireAdminRole/RequireAnyRole are built in router.go from
// tenant.RequireRole(tenant.RoleAdmin/RoleStaff) - this package does not
// import internal/tenant directly, matching §9's import-direction note.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service, origins *originpolicy.Policy) {
	h := NewHandler(svc, origins)

	// ---- user-session authenticated, tenant-scoped ----
	mux.Handle("POST /api/v1/events", routing.Chain(h.Create, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events", routing.Chain(h.List, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}", routing.Chain(h.Get, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PATCH /api/v1/events/{id}", routing.Chain(h.Update, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PATCH /api/v1/events/{id}/status", routing.Chain(h.SetStatus, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))

	mux.Handle("POST /api/v1/events/{id}/races", routing.Chain(h.CreateRace, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}/races", routing.Chain(h.ListRaces, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PATCH /api/v1/events/{id}/races/{raceId}", routing.Chain(h.UpdateRace, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/events/{id}/races/{raceId}", routing.Chain(h.DeleteRace, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))

	// ---- event-scoped crew/volunteer access (docs/event-crew-access-plan.md) ----
	//
	// Assignment *management* is tenant-staff-only, gated exactly like
	// every other event-scoped route above (RequireAnyRole = Staff+).
	mux.Handle("POST /api/v1/events/{id}/assignments", routing.Chain(h.CreateAssignment, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}/assignments", routing.Chain(h.ListAssignments, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PATCH /api/v1/events/{id}/assignments/{assignmentId}", routing.Chain(h.UpdateAssignment, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/events/{id}/assignments/{assignmentId}", routing.Chain(h.RevokeAssignment, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))

	// The self-check endpoint is the one real consumer of svc.
	// RequireEventAccess - deliberately NOT mw.RequireTenantForUser in
	// this chain, see that middleware's own doc comment (access.go) for
	// why: it resolves tenant context itself, and accepts a caller who
	// has no tenant_members row here at all.
	mux.Handle("GET /api/v1/events/{id}/assignments/me", routing.Chain(h.MyAssignmentOnEvent, svc.RequireEventAccess(""), mw.RequireUserAuth))

	// ---- event-scoped invitation acceptance + cross-tenant "my events" (no tenant context resolved yet) ----
	mux.Handle("POST /api/v1/events/invitations/accept", routing.Chain(h.AcceptEventInvitation, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/me/event-assignments", routing.Chain(h.MyEventAssignments, mw.RequireUserAuth))

	// ---- M2M, read-only ----
	mux.Handle("GET /api/v1/m2m/events", routing.Chain(
		h.List,
		middleware.RequireScope(oauthclient.ScopeEventsRead),
		middleware.RequireTenantForM2M,
		mw.RequireM2MAuth,
	))
	mux.Handle("GET /api/v1/m2m/events/{id}", routing.Chain(
		h.Get,
		middleware.RequireScope(oauthclient.ScopeEventsRead),
		middleware.RequireTenantForM2M,
		mw.RequireM2MAuth,
	))
}
