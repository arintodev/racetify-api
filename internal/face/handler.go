package face

import (
	"errors"
	"io"
	"net/http"

	"github.com/racetify/racetify-api/internal/gallery"
	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
)

// CapabilityFaceSearch mirrors internal/event's constant (kept as a string
// so this package does not import internal/event, the same reasoning
// gallery.CapabilityUpload/CapabilityReview already use).
const CapabilityFaceSearch = "gallery:face_search"

// maxEnrollImageBytes bounds one enrollment upload - a single portrait
// photo, not a batch, so this is much smaller than gallery.MaxInputBytes.
const maxEnrollImageBytes = 10 << 20

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// fail writes err: a face.Error carries its own status, code and field;
// anything else goes through the shared mapping - same shape as
// gallery.Handler's fail.
func fail(w http.ResponseWriter, err error) {
	var ferr *Error
	if errors.As(err, &ferr) {
		respond.ErrorWithField(w, ferr.Status, ferr.Code, ferr.Message, ferr.Field)
		return
	}
	respond.FromServiceError(w, err)
}

func actor(r *http.Request) (tenantID, userID string) {
	tenantID, _ = reqctx.TenantID(r.Context())
	userID, _ = reqctx.UserID(r.Context())
	return tenantID, userID
}

// Enroll handles POST /events/{id}/users/{uid}/face-embeddings.
func (h *Handler) Enroll(w http.ResponseWriter, r *http.Request) {
	tenantID, actorUserID := actor(r)
	userID := r.PathValue("uid")
	if !isUUID(userID) {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "uid is not valid.", "uid")
		return
	}

	if err := r.ParseMultipartForm(maxEnrollImageBytes); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "Request must be a multipart form with an image file.")
		return
	}
	consent := r.FormValue("consent") == "true"
	file, header, err := r.FormFile("image")
	if err != nil {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "image is required.", "image")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxEnrollImageBytes+1))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "Could not read the uploaded image.")
		return
	}
	if len(data) > maxEnrollImageBytes {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "image exceeds 10 MB.", "image")
		return
	}

	face, err := h.svc.Enroll(r.Context(), tenantID, r.PathValue("id"), userID, actorUserID, data, header.Filename, consent)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, faceResponse(face))
}

// ListEmbeddings handles GET /events/{id}/users/{uid}/face-embeddings.
func (h *Handler) ListEmbeddings(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := actor(r)
	userID := r.PathValue("uid")
	if !isUUID(userID) {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "uid is not valid.", "uid")
		return
	}
	face, embeddings, err := h.svc.ListEmbeddings(r.Context(), tenantID, r.PathValue("id"), userID)
	if err != nil {
		fail(w, err)
		return
	}
	dto := ListEmbeddingsDTO{Embeddings: make([]EmbeddingDTO, len(embeddings))}
	if face != nil {
		f := faceResponse(face)
		dto.Face = &f
	}
	for i := range embeddings {
		dto.Embeddings[i] = embeddingResponse(&embeddings[i])
	}
	respond.JSON(w, http.StatusOK, dto)
}

// RevokeConsent handles DELETE /events/{id}/faces/{fid}/consent.
func (h *Handler) RevokeConsent(w http.ResponseWriter, r *http.Request) {
	tenantID, actorUserID := actor(r)
	if err := h.svc.RevokeConsent(r.Context(), tenantID, r.PathValue("fid"), actorUserID); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Search handles POST /events/{id}/faces/search.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := actor(r)
	var req searchRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if !isUUID(req.FaceID) {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "face_id is not valid.", "face_id")
		return
	}
	page, err := h.svc.Search(r.Context(), tenantID, r.PathValue("id"), req.FaceID, respond.PageParamsFromRequest(r))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]gallery.PhotoDTO, len(page.Items))
	for i := range page.Items {
		dto, err := h.svc.PhotoResponse(&page.Items[i])
		if err != nil {
			fail(w, err)
			return
		}
		out[i] = dto
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[gallery.PhotoDTO]{Items: out, NextCursor: page.NextCursor})
}
