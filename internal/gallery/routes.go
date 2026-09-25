package gallery

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

// RegisterRoutes wires the gallery endpoints. Routes are event-scoped so
// RequireEventAccess can resolve the tenant from the event id.
//
// Reads admit tenant staff or any crew member with a gallery capability.
// Album writes stay with tenant staff (deleting with admins), through the
// ordinary tenant-scoped chain; the service re-checks the role.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service, gate EventGate) {
	h := NewHandler(svc)
	// "" admits any active assignment; the handler narrows it to the
	// gallery capabilities.
	anyCrew := gate.RequireEventAccess("")

	mux.Handle("GET /api/v1/events/{id}/albums", routing.Chain(requireAnyGalleryCapability(h.ListAlbums), anyCrew, mw.RequireUserAuth))

	mux.Handle("GET /api/v1/events/{id}/photos", routing.Chain(requireAnyGalleryCapability(h.ListPhotos), anyCrew, mw.RequireUserAuth))

	// Uploading admits tenant staff or a crew member holding gallery:upload;
	// storage's own upload endpoint is admin-only, so the gallery provisions
	// the upload URLs itself.
	upload := gate.RequireEventAccess(CapabilityUpload)
	mux.Handle("POST /api/v1/events/{id}/albums/{aid}/photos/upload-urls", routing.Chain(h.UploadURLs, upload, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/albums/{aid}/photos/complete", routing.Chain(h.Complete, upload, mw.RequireUserAuth))

	mux.Handle("POST /api/v1/events/{id}/albums", routing.Chain(h.CreateAlbum, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PATCH /api/v1/events/{id}/albums/{aid}", routing.Chain(h.UpdateAlbum, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/events/{id}/albums/{aid}", routing.Chain(h.DeleteAlbum, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
}
