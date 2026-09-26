package certificate

import (
	"errors"
	"net/http"
	"time"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// maxBodyBytes caps a request body: the image itself travels through Object
// Storage, not this API.
const maxBodyBytes = 16 << 10

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// fail writes err: a certificate.Error carries its own status, code and
// field; anything else goes through the shared mapping.
func fail(w http.ResponseWriter, err error) {
	var cerr *Error
	if errors.As(err, &cerr) {
		respond.ErrorWithField(w, cerr.Status, cerr.Code, cerr.Message, cerr.Field)
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

func (h *Handler) dto(c *Certificate) (CertificateDTO, error) {
	url, expires, err := h.svc.FileURL(c)
	if err != nil {
		return CertificateDTO{}, err
	}
	return certificateResponse(c, url, expires), nil
}

// List handles GET /events/{id}/certificates.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	listing, err := h.svc.List(r.Context(), tenantID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	items := make([]CertificateDTO, len(listing.Items))
	for i := range listing.Items {
		if items[i], err = h.dto(&listing.Items[i]); err != nil {
			fail(w, err)
			return
		}
	}
	respond.JSON(w, http.StatusOK, ListDTO{Items: items, PublishedAt: listing.PublishedAt})
}

// Upsert handles PUT /events/{id}/certificates/{pid}.
func (h *Handler) Upsert(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req upsertRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	c, err := h.svc.Upsert(r.Context(), tenantID, r.PathValue("id"), r.PathValue("pid"), userID, role, req.input())
	if err != nil {
		fail(w, err)
		return
	}
	out, err := h.dto(c)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, out)
}

// Delete handles DELETE /events/{id}/certificates/{pid}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	if err := h.svc.Delete(r.Context(), tenantID, r.PathValue("id"), r.PathValue("pid"), userID, role); err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// Publish handles POST /events/{id}/certificates/publish.
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		Published bool `json:"published"`
	}
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	at, err := h.svc.SetPublished(r.Context(), tenantID, r.PathValue("id"), userID, role, req.Published)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, struct {
		PublishedAt *time.Time `json:"published_at"`
	}{at})
}

// maxBatchBodyBytes caps a generate request: up to MaxBatch certificates,
// each with the values of its tags.
const maxBatchBodyBytes = 16 << 20

// Generate handles POST /events/{id}/certificates/generate.
func (h *Handler) Generate(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchBodyBytes)
	var req struct {
		TemplateID string      `json:"template_id"`
		Format     string      `json:"format"`
		DPI        int         `json:"dpi"`
		Items      []BatchItem `json:"items"`
		ScopeLabel string      `json:"scope_label"`
	}
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	jobID, err := h.svc.StartBatch(r.Context(), tenantID, r.PathValue("id"), userID, role, BatchInput{
		TemplateID: req.TemplateID, Format: req.Format, DPI: req.DPI, Items: req.Items, ScopeLabel: req.ScopeLabel,
	})
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}
