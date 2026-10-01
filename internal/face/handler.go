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

// readEnrollImage parses a multipart enrollment request's shared fields:
// the image file (required) and the "consent" flag. Used by both Enroll
// and EnrollByRef, which otherwise only differ in whose face is being
// enrolled and what audit metadata that implies.
func readEnrollImage(w http.ResponseWriter, r *http.Request) (data []byte, consent bool, ok bool) {
	if err := r.ParseMultipartForm(maxEnrollImageBytes); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "Request must be a multipart form with an image file.")
		return nil, false, false
	}
	consent = r.FormValue("consent") == "true"
	file, _, err := r.FormFile("image")
	if err != nil {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "image is required.", "image")
		return nil, false, false
	}
	defer file.Close()
	data, err = io.ReadAll(io.LimitReader(file, maxEnrollImageBytes+1))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "Could not read the uploaded image.")
		return nil, false, false
	}
	if len(data) > maxEnrollImageBytes {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "image exceeds 10 MB.", "image")
		return nil, false, false
	}
	return data, consent, true
}

// ==================== self-enroll (Racetify user) ====================

// Enroll handles POST /users/{uid}/face-enrollment. uid must be the
// caller's own id - there is no tenant/event gate on this route any more
// to borrow authorization from (docs/face-tenant-enrollment-plan.md §5),
// so self-ness is enforced here instead.
func (h *Handler) Enroll(w http.ResponseWriter, r *http.Request) {
	actorUserID, _ := reqctx.UserID(r.Context())
	uid := r.PathValue("uid")
	if uid != actorUserID {
		respond.Error(w, http.StatusForbidden, "forbidden", "You can only enroll your own face.")
		return
	}
	data, consent, ok := readEnrollImage(w, r)
	if !ok {
		return
	}
	face, err := h.svc.Enroll(r.Context(), uid, actorUserID, data, consent)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, faceResponse(face))
}

// ListEmbeddings handles GET /users/{uid}/face-enrollment.
func (h *Handler) ListEmbeddings(w http.ResponseWriter, r *http.Request) {
	actorUserID, _ := reqctx.UserID(r.Context())
	uid := r.PathValue("uid")
	if uid != actorUserID {
		respond.Error(w, http.StatusForbidden, "forbidden", "You can only view your own face.")
		return
	}
	face, embeddings, err := h.svc.ListEmbeddings(r.Context(), uid)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, listEmbeddingsResponse(face, embeddings))
}

// DropFace handles DELETE /faces/{fid} for the self-enroll path.
func (h *Handler) DropFace(w http.ResponseWriter, r *http.Request) {
	actorUserID, _ := reqctx.UserID(r.Context())
	if err := h.svc.DropOwnFace(r.Context(), actorUserID, r.PathValue("fid")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ==================== tenant M2M ====================

// EnrollByRef handles POST /clients/face-enrollment. ref_id and
// consent_reference are additional multipart form fields on top of what
// readEnrollImage already reads.
func (h *Handler) EnrollByRef(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorClientID, _ := reqctx.M2MClientID(r.Context())

	data, consent, ok := readEnrollImage(w, r)
	if !ok {
		return
	}
	refID := r.FormValue("ref_id")
	if refID == "" {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "ref_id is required.", "ref_id")
		return
	}
	consentReference := r.FormValue("consent_reference")

	face, err := h.svc.EnrollByRef(r.Context(), tenantID, refID, actorClientID, data, consent, consentReference)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, faceResponse(face))
}

// ListEmbeddingsByRef handles GET /clients/face-enrollment?ref_id=...
func (h *Handler) ListEmbeddingsByRef(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	refID := r.URL.Query().Get("ref_id")
	if refID == "" {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "ref_id is required.", "ref_id")
		return
	}
	face, embeddings, err := h.svc.ListEmbeddingsByRef(r.Context(), tenantID, refID)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, listEmbeddingsResponse(face, embeddings))
}

// DropFaceByRef handles DELETE /clients/faces/{fid}.
func (h *Handler) DropFaceByRef(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorClientID, _ := reqctx.M2MClientID(r.Context())
	if err := h.svc.DropFaceByRef(r.Context(), tenantID, actorClientID, r.PathValue("fid")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ==================== search (shared) ====================

// Search handles both POST /events/{id}/faces/search (user-session) and
// POST /clients/events/{id}/faces/search (M2M) - tenantID comes from
// reqctx regardless of which route's middleware set it
// (event.RequireEventAccess for the former, middleware.RequireTenantForM2M
// for the latter), so one handler serves both (docs/face-tenant-
// enrollment-plan.md §5).
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
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
