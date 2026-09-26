package bibprint

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/jobqueue"
)

// RegisterRoutes wires the BIB print endpoints (Owner/Admin only: printing
// puts personal data on paper). The job's progress and result are read with
// the generic GET /jobs/{id}.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service) {
	h := NewHandler(svc)

	mux.Handle("POST /api/v1/events/{id}/bib-generation", routing.Chain(h.Start, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/events/{id}/bib-generation/{jobId}/download", routing.Chain(h.Download, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
}

// RegisterJob registers the worker's handler for BIB print jobs.
func RegisterJob(d *jobqueue.Dispatcher, svc *Service) {
	d.Register(domain.JobTypeGeneratorBibBatch, svc.Run)
}
