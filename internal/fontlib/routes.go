package fontlib

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

// RegisterRoutes wires the font library. Reading (the list and the files a
// browser needs to draw them) is open to every signed-in user; changing the
// library is for platform administrators only.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service) {
	h := NewHandler(svc)

	mux.Handle("GET /api/v1/fonts", routing.Chain(h.List, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/fonts/files/{id}/{style}", routing.Chain(h.File, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/fonts/bundled/{name}/{style}", routing.Chain(h.BundledFile, mw.RequireUserAuth))

	admin := func(handler http.HandlerFunc) http.Handler {
		return routing.Chain(handler, mw.RequireSuperAdmin, mw.RequireUserAuth)
	}
	mux.Handle("GET /api/v1/admin/fonts", admin(h.AdminList))
	mux.Handle("POST /api/v1/admin/fonts", admin(h.Create))
	mux.Handle("GET /api/v1/admin/fonts/demand", admin(h.Demand))
	mux.Handle("PATCH /api/v1/admin/fonts/{id}", admin(h.Patch))
	mux.Handle("DELETE /api/v1/admin/fonts/{id}", admin(h.Delete))
	mux.Handle("GET /api/v1/admin/fonts/{id}/usage", admin(h.Uses))
	mux.Handle("PUT /api/v1/admin/fonts/{id}/files/{style}", admin(h.PutFile))
	mux.Handle("DELETE /api/v1/admin/fonts/{id}/files/{style}", admin(h.RemoveFile))
}
