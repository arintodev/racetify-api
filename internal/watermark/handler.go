package watermark

import (
	"errors"
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// fail writes err: a watermark.Error carries its own status, code and
// field; anything else goes through the shared mapping.
func fail(w http.ResponseWriter, err error) {
	var werr *Error
	if errors.As(err, &werr) {
		respond.ErrorWithField(w, werr.Status, werr.Code, werr.Message, werr.Field)
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

func (h *Handler) toDTOs(w http.ResponseWriter, items []*Watermark) ([]WatermarkDTO, bool) {
	out := make([]WatermarkDTO, len(items))
	for i := range items {
		dto, err := h.svc.watermarkResponse(items[i])
		if err != nil {
			fail(w, err)
			return nil, false
		}
		out[i] = dto
	}
	return out, true
}

// List handles GET /events/{id}/watermarks.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _, role := actor(r)
	items, err := h.svc.List(r.Context(), tenantID, r.PathValue("id"), role)
	if err != nil {
		fail(w, err)
		return
	}
	out, ok := h.toDTOs(w, items)
	if !ok {
		return
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[WatermarkDTO]{Items: out})
}

// Replace handles PUT /events/{id}/watermarks: it replaces the event's
// entire set of watermark layers.
func (h *Handler) Replace(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req replaceRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	items := make([]ReplaceItem, len(req.Watermarks))
	for i, it := range req.Watermarks {
		var id string
		if it.ID != nil {
			id = *it.ID
		}
		items[i] = ReplaceItem{
			ID: id, StorageID: it.StorageID, Name: it.Name, AnchorX: it.AnchorX, AnchorY: it.AnchorY,
			OffsetXPercent: it.OffsetXPercent, OffsetYPercent: it.OffsetYPercent,
			WidthPercent: it.WidthPercent, AspectRatio: it.AspectRatio, Opacity: it.Opacity, SortOrder: it.SortOrder,
		}
	}
	updated, err := h.svc.ReplaceAll(r.Context(), tenantID, r.PathValue("id"), userID, role, items)
	if err != nil {
		fail(w, err)
		return
	}
	out, ok := h.toDTOs(w, updated)
	if !ok {
		return
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[WatermarkDTO]{Items: out})
}
