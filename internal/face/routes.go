package face

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/middleware"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/oauthclient"
)

// EventGate is what routes need from the event package to admit an
// external crew member with a capability - same interface shape as
// gallery.EventGate, kept separate rather than shared since each package
// deliberately does not import internal/event.
type EventGate interface {
	RequireEventAccess(capability string) func(http.Handler) http.Handler
}

// RegisterRoutes wires the face endpoints (docs/face-tenant-enrollment-
// plan.md §5): self-enroll/list/drop for a Racetify user (no tenant/event
// in the path at all - a user's face is their own, globally), the tenant
// M2M equivalents keyed by ref_id (Client Credentials grant,
// internal/oauthclient), and search, which both paths share since it only
// needs whichever tenant_id/event_id reqctx already carries.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service, gate EventGate) {
	h := NewHandler(svc)
	faceSearch := gate.RequireEventAccess(CapabilityFaceSearch)

	// ---- self-enroll (Racetify user, no tenant) ----
	mux.Handle("POST /api/v1/users/{uid}/face-enrollment", routing.Chain(h.Enroll, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/users/{uid}/face-enrollment", routing.Chain(h.ListEmbeddings, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/faces/{fid}", routing.Chain(h.DropFace, mw.RequireUserAuth))

	// ---- tenant M2M (Client Credentials grant) ----
	mux.Handle("POST /api/v1/clients/face-enrollment", routing.Chain(h.EnrollByRef,
		middleware.RequireScope(oauthclient.ScopeFaceWrite), middleware.RequireTenantForM2M, mw.RequireM2MAuth))
	mux.Handle("GET /api/v1/clients/face-enrollment", routing.Chain(h.ListEmbeddingsByRef,
		middleware.RequireScope(oauthclient.ScopeFaceRead), middleware.RequireTenantForM2M, mw.RequireM2MAuth))
	mux.Handle("DELETE /api/v1/clients/faces/{fid}", routing.Chain(h.DropFaceByRef,
		middleware.RequireScope(oauthclient.ScopeFaceWrite), middleware.RequireTenantForM2M, mw.RequireM2MAuth))

	// ---- search (shared handler, see Handler.Search's doc comment) ----
	mux.Handle("POST /api/v1/events/{id}/faces/search", routing.Chain(h.Search, faceSearch, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/clients/events/{id}/faces/search", routing.Chain(h.Search,
		middleware.RequireScope(oauthclient.ScopeFaceRead), middleware.RequireTenantForM2M, mw.RequireM2MAuth))
}
