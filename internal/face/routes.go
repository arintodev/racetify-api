package face

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

// EventGate is what routes need from the event package to admit an
// external crew member with a capability - same interface shape as
// gallery.EventGate, kept separate rather than shared since each package
// deliberately does not import internal/event.
type EventGate interface {
	RequireEventAccess(capability string) func(http.Handler) http.Handler
}

// RegisterRoutes wires the face endpoints. Routes are event-scoped, same
// reasoning as gallery.RegisterRoutes: RequireEventAccess resolves the
// tenant from the event id, and every route needs CapabilityFaceSearch or
// tenant staff.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service, gate EventGate) {
	h := NewHandler(svc)
	faceSearch := gate.RequireEventAccess(CapabilityFaceSearch)

	mux.Handle("POST /api/v1/events/{id}/users/{uid}/face-embeddings", routing.Chain(h.Enroll, faceSearch, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}/users/{uid}/face-embeddings", routing.Chain(h.ListEmbeddings, faceSearch, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/events/{id}/faces/{fid}/consent", routing.Chain(h.RevokeConsent, faceSearch, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/faces/search", routing.Chain(h.Search, faceSearch, mw.RequireUserAuth))
}
