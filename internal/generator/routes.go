package generator

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

// RegisterRoutes wires the template endpoints (docs/phase1-api-plan.md §4.3).
// Reading is open to workspace staff; changing templates is Owner/Admin only
// (docs/cetak-bib-ux.md §6). The SVG bytes themselves go through the Object
// Storage endpoints, and a template is registered against the uploaded
// object's id.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service) {
	h := NewHandler(svc)

	mux.Handle("GET /api/v1/events/{id}/templates", routing.Chain(h.List, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}/templates/{tid}", routing.Chain(h.Get, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/templates", routing.Chain(h.Create, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PATCH /api/v1/events/{id}/templates/{tid}", routing.Chain(h.Update, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/events/{id}/templates/{tid}", routing.Chain(h.Delete, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
}
