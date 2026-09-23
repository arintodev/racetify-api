package jobqueue

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
)

// Handler serves the generic, read-only job-status polling endpoints
// (docs/phase1-api-plan.md §4.5) shared by every async flow - CSV import,
// BIB batch generation, photo processing. It wraps Queue directly (the
// same type that owns Enqueue/Dequeue) rather than a separate Service,
// since polling is a thin, read-only pass-through with no invariant of its
// own to enforce.
type Handler struct {
	queue *Queue
}

func NewHandler(queue *Queue) *Handler {
	return &Handler{queue: queue}
}

// Get handles GET /api/v1/jobs/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	job, err := h.queue.GetJob(r.Context(), tenantID, r.PathValue("id"))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, jobResponse(job))
}

// List handles GET /api/v1/jobs. Paginated: ?limit=&cursor=, with optional
// ?type= and ?status= filters.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())

	var jobType *domain.JobType
	if raw := r.URL.Query().Get("type"); raw != "" {
		t := domain.JobType(raw)
		jobType = &t
	}
	var status *domain.JobStatus
	if raw := r.URL.Query().Get("status"); raw != "" {
		s := domain.JobStatus(raw)
		status = &s
	}

	page, err := h.queue.ListJobs(r.Context(), tenantID, respond.PageParamsFromRequest(r), jobType, status)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]JobDTO, 0, len(page.Items))
	for i := range page.Items {
		out = append(out, jobResponse(&page.Items[i]))
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[JobDTO]{Items: out, NextCursor: page.NextCursor})
}
