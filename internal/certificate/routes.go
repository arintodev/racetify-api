package certificate

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/jobqueue"
)

// RegisterRoutes wires the certificate endpoints. Reading is open to
// workspace staff; changing certificates and publishing is Owner/Admin only
// (docs/e-certificate-ux.md §7: crew never reach certificate files). The
// image bytes themselves go through the Object Storage endpoints, and a
// certificate is registered against the uploaded object's id.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service) {
	h := NewHandler(svc)

	mux.Handle("GET /api/v1/events/{id}/certificates", routing.Chain(h.List, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/certificates/generate", routing.Chain(h.Generate, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("POST /api/v1/events/{id}/certificates/publish", routing.Chain(h.Publish, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("PUT /api/v1/events/{id}/certificates/{pid}", routing.Chain(h.Upsert, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("DELETE /api/v1/events/{id}/certificates/{pid}", routing.Chain(h.Delete, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
}

// RegisterJob registers the worker's handler for certificate batches.
func RegisterJob(d *jobqueue.Dispatcher, svc *Service) {
	d.Register(domain.JobTypeCertificatesBatch, svc.RunBatch)
}
