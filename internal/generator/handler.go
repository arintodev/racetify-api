package generator

import (
	"errors"
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// maxBodyBytes caps a template request body: metadata is at most 64 KiB, the
// SVG itself travels through Object Storage, not this API.
const maxBodyBytes = 128 << 10

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// fail writes err: a generator.Error carries its own status, code and field;
// anything else goes through the shared mapping.
func fail(w http.ResponseWriter, err error) {
	var gerr *Error
	if errors.As(err, &gerr) {
		respond.ErrorWithField(w, gerr.Status, gerr.Code, gerr.Message, gerr.Field)
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

func (h *Handler) respond(w http.ResponseWriter, status int, t *Template) {
	url, expires, err := h.svc.SVGURL(t)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, status, templateResponse(t, url, expires))
}

// List handles GET /events/{id}/templates[?service=bib|certificate].
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	items, err := h.svc.List(r.Context(), tenantID, r.PathValue("id"), Kind(r.URL.Query().Get("service")))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]TemplateDTO, len(items))
	for i := range items {
		url, expires, err := h.svc.SVGURL(&items[i])
		if err != nil {
			fail(w, err)
			return
		}
		out[i] = templateResponse(&items[i], url, expires)
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[TemplateDTO]{Items: out})
}

// Get handles GET /events/{id}/templates/{tid}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	t, err := h.svc.Get(r.Context(), tenantID, r.PathValue("id"), r.PathValue("tid"))
	if err != nil {
		fail(w, err)
		return
	}
	h.respond(w, http.StatusOK, t)
}

// Create handles POST /events/{id}/templates.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req createRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	in := CreateInput{
		Kind: Kind(req.Service), Name: req.Name, StorageID: req.StorageID,
		Active: req.IsActive == nil || *req.IsActive, Metadata: req.Metadata,
	}
	if req.RaceID != "" {
		in.RaceID = &req.RaceID
	}
	t, err := h.svc.Create(r.Context(), tenantID, r.PathValue("id"), userID, role, in)
	if err != nil {
		fail(w, err)
		return
	}
	h.respond(w, http.StatusCreated, t)
}

// Update handles PATCH /events/{id}/templates/{tid}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req updateRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	t, err := h.svc.Update(r.Context(), tenantID, r.PathValue("id"), r.PathValue("tid"), userID, role, PatchInput{
		Name: req.Name, StorageID: req.StorageID, RaceID: req.RaceID, Active: req.IsActive, Metadata: req.Metadata,
	})
	if err != nil {
		fail(w, err)
		return
	}
	h.respond(w, http.StatusOK, t)
}

// Delete handles DELETE /events/{id}/templates/{tid}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	if err := h.svc.Delete(r.Context(), tenantID, r.PathValue("id"), r.PathValue("tid"), userID, role); err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}
