package portal

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

// RegisterRoutes mounts the Runner Portal surface built so far: event info
// and e-certificate search/download. No auth middleware at all - only
// rate limiting, since there is no credential to key off
// (docs/phase1-api-plan.md §5's "no login plus search by BIB or name is
// also, unavoidably, an enumeration surface" note).
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service) {
	h := NewHandler(svc)
	limit := mw.PortalRateLimit

	mux.Handle("GET /api/v1/public/events/{slug}", routing.Chain(h.GetEvent, limit))
	mux.Handle("GET /api/v1/public/events/{slug}/certificates/search", routing.Chain(h.SearchCertificates, limit))
	mux.Handle("GET /api/v1/public/events/{slug}/certificates/{pid}/download", routing.Chain(h.DownloadCertificate, limit))

	mux.Handle("GET /api/v1/public/events/{slug}/gallery/albums", routing.Chain(h.ListAlbums, limit))
	mux.Handle("GET /api/v1/public/events/{slug}/gallery/photos", routing.Chain(h.SearchPhotos, limit))
	mux.Handle("GET /api/v1/public/events/{slug}/gallery/albums/{aid}/photos", routing.Chain(h.ListAlbumPhotos, limit))
}
