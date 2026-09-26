package bibprint

import (
	"errors"
	"net/http"
	"time"

	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// maxBodyBytes caps a print request: up to MaxPeople participant ids.
const maxBodyBytes = 2 << 20

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func fail(w http.ResponseWriter, err error) {
	var berr *Error
	if errors.As(err, &berr) {
		respond.ErrorWithField(w, berr.Status, berr.Code, berr.Message, berr.Field)
		return
	}
	respond.FromServiceError(w, err)
}

func actor(r *http.Request) (tenantID, userID string, role rbac.MemberRole) {
	tenantID, _ = reqctx.TenantID(r.Context())
	userID, _ = reqctx.UserID(r.Context())
	roleStr, _ := reqctx.MemberRole(r.Context())
	return tenantID, userID, rbac.MemberRole(roleStr)
}

type startRequest struct {
	TemplateID     string           `json:"template_id"`
	Layout         generator.Layout `json:"layout"`
	ParticipantIDs []string         `json:"participant_ids"`
	ScopeLabel     string           `json:"scope_label"`
}

// Start handles POST /events/{id}/bib-generation.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req startRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	jobID, err := h.svc.Start(r.Context(), tenantID, r.PathValue("id"), userID, role, StartInput{
		TemplateID: req.TemplateID, Layout: req.Layout, ParticipantIDs: req.ParticipantIDs, ScopeLabel: req.ScopeLabel,
	})
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

// Download handles GET /events/{id}/bib-generation/{jobId}/download.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	tenantID, _, role := actor(r)
	d, err := h.svc.Download(r.Context(), tenantID, r.PathValue("id"), r.PathValue("jobId"), role)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, struct {
		URL       string     `json:"url"`
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
		FileName  string     `json:"file_name"`
	}{d.URL, d.ExpiresAt, d.FileName})
}
