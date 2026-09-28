package watermark

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

// RegisterRoutes wires the watermark endpoints. Reading is Staff+, replacing
// the set is Admin-only - the same gate generator.RegisterRoutes uses for
// template writes (docs/cetak-bib-ux.md §6), since a watermark is workspace
// branding configuration, not something every crew member touches.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service) {
	h := NewHandler(svc)

	mux.Handle("GET /api/v1/events/{id}/watermarks", routing.Chain(h.List, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PUT /api/v1/events/{id}/watermarks", routing.Chain(h.Replace, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
}
