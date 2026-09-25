package participant

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

// EventGate is what routes need from the event package to admit an external
// crew member with a capability (RequireEventAccess); an interface so this
// package does not import internal/event.
type EventGate interface {
	RequireEventAccess(capability string) func(http.Handler) http.Handler
}

// CapabilityParticipantsRead is the crew capability that allows reading an
// event's participants (mirrors internal/event's capability of the same name).
const CapabilityParticipantsRead = "participants:read"

// RegisterRoutes wires participant and team endpoints (docs/participants-
// integration.md §3.1).
//
// Reads admit either tenant staff (Staff+) or an external crew member whose
// assignment carries participants:read - RequireEventAccess resolves the
// tenant from the event id and checks both. Writes stay with tenant staff,
// via the ordinary tenant-scoped chain, with the service re-checking roles.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service, gate EventGate) {
	h := NewHandler(svc)
	read := gate.RequireEventAccess(CapabilityParticipantsRead)

	// ---- reads (staff or crew with participants:read) ----
	mux.Handle("GET /api/v1/events/{id}/participants", routing.Chain(h.List, read, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}/participants/summary", routing.Chain(h.Summary, read, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}/participants/{pid}", routing.Chain(h.Get, read, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}/teams", routing.Chain(h.ListTeams, read, mw.RequireUserAuth))

	// ---- tenant staff ----
	// Export sends personal data out of the system: staff only, never crew.
	mux.Handle("GET /api/v1/events/{id}/participants/export", routing.Chain(h.Export, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/participants", routing.Chain(h.Create, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PATCH /api/v1/events/{id}/participants/{pid}", routing.Chain(h.Update, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/events/{id}/participants/{pid}", routing.Chain(h.Delete, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/participants/bulk-delete", routing.Chain(h.BulkDelete, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/participants/bulk-status", routing.Chain(h.BulkStatus, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/participants/import", routing.Chain(h.Import, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))

	mux.Handle("POST /api/v1/events/{id}/teams", routing.Chain(h.CreateTeam, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PATCH /api/v1/events/{id}/teams/{tid}", routing.Chain(h.UpdateTeam, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/events/{id}/teams/{tid}", routing.Chain(h.DeleteTeam, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
}
