package jobqueue

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

// RegisterRoutes wires the read-only job-status polling endpoints
// (docs/phase1-api-plan.md §4.5, routes_job.go in the plan's route-file
// list). Any active tenant member may poll a job's status - it is a read,
// gated the same way every other Staff-level read in this codebase is
// (mw.RequireAnyRole), not the RoleAdmin+ gate the provisioning action
// that *created* the job used.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, queue *Queue) {
	h := NewHandler(queue)

	mux.Handle("GET /api/v1/jobs/{id}", routing.Chain(h.Get, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/jobs", routing.Chain(h.List, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
}
