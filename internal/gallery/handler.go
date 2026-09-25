package gallery

import (
	"errors"
	"net/http"
	"slices"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// Capabilities that admit an external crew member to the gallery (mirror
// internal/event's constants; kept as strings so this package does not
// import internal/event).
const (
	CapabilityUpload = "gallery:upload"
	CapabilityReview = "gallery:review"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// fail writes err: a gallery.Error carries its own status, code and field;
// anything else goes through the shared mapping.
func fail(w http.ResponseWriter, err error) {
	var gerr *Error
	if errors.As(err, &gerr) {
		respond.ErrorWithField(w, gerr.Status, gerr.Code, gerr.Message, gerr.Field)
		return
	}
	respond.FromServiceError(w, err)
}

// actor extracts who is calling. tenant is always present once the route's
// gate has run; role is empty for an external crew member.
func actor(r *http.Request) (tenantID, userID string, role rbac.MemberRole) {
	tenantID, _ = reqctx.TenantID(r.Context())
	userID, _ = reqctx.UserID(r.Context())
	roleStr, _ := reqctx.MemberRole(r.Context())
	return tenantID, userID, rbac.MemberRole(roleStr)
}

// hasCapability reports whether the caller may act on the gallery with the
// given capability: tenant staff always may, an external crew member only
// with the capability on their assignment.
func hasCapability(r *http.Request, capability string) bool {
	if role, ok := reqctx.MemberRole(r.Context()); ok && rbac.MemberRole(role).IsAtLeast(rbac.RoleStaff) {
		return true
	}
	caps, _ := reqctx.EventCapabilities(r.Context())
	return slices.Contains(caps, capability)
}

// requireAnyGalleryCapability admits staff or a crew member with either
// gallery capability. Album lists are needed by uploaders and reviewers
// alike, and RequireEventAccess takes one capability at most.
func requireAnyGalleryCapability(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hasCapability(r, CapabilityUpload) && !hasCapability(r, CapabilityReview) {
			respond.Error(w, http.StatusForbidden, "forbidden", "You do not have permission to perform this action.")
			return
		}
		next(w, r)
	}
}

// ==================== upload ====================

// UploadURLs handles POST /events/{id}/albums/{aid}/photos/upload-urls.
func (h *Handler) UploadURLs(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	userID, _ := reqctx.UserID(r.Context())
	var req uploadURLsRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	entries := make([]UploadEntry, len(req.Files))
	for i, f := range req.Files {
		entries[i] = UploadEntry{Filename: f.Filename, OriginalSize: f.OriginalSize}
	}
	results, err := h.svc.RequestUploadURLs(r.Context(), tenantID, r.PathValue("id"), r.PathValue("aid"), userID, entries)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]UploadURLDTO, len(results))
	for i, res := range results {
		out[i] = UploadURLDTO{Filename: entries[i].Filename, StorageID: res.StorageID, UploadURL: res.UploadURL, Skipped: res.Skipped}
		if !res.ExpiresAt.IsZero() {
			exp := res.ExpiresAt
			out[i].ExpiresAt = &exp
		}
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[UploadURLDTO]{Items: out})
}

// Complete handles POST /events/{id}/albums/{aid}/photos/complete.
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	userID, _ := reqctx.UserID(r.Context())
	var req completeRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	items := make([]CompleteItem, len(req.Items))
	for i, it := range req.Items {
		items[i] = CompleteItem{
			StorageID: it.StorageID, OriginalFilename: it.OriginalFilename, OriginalSize: it.OriginalSize,
			Width: it.Width, Height: it.Height,
		}
	}
	results, err := h.svc.CompleteUploads(r.Context(), tenantID, r.PathValue("id"), r.PathValue("aid"), userID, items)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]CompleteItemDTO, len(results))
	for i, res := range results {
		out[i] = CompleteItemDTO{StorageID: res.StorageID, PhotoID: res.PhotoID, Status: res.Status, Reason: res.Reason}
	}
	respond.JSON(w, http.StatusCreated, CompleteDTO{Items: out})
}

// ==================== photos ====================

// ListPhotos handles GET /events/{id}/photos. Crew with only gallery:upload
// see their own uploads, whatever the query says.
func (h *Handler) ListPhotos(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, _ := actor(r)
	q := r.URL.Query()
	f := PhotoFilter{AlbumID: q.Get("album_id"), State: State(q.Get("state")), BIB: q.Get("bib"), UploaderID: q.Get("uploader_id")}
	if f.UploaderID != "" && !isUUID(f.UploaderID) {
		respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "uploader_id is not valid.", "uploader_id")
		return
	}
	if q.Get("mine") == "true" || !hasCapability(r, CapabilityReview) {
		f.UploaderID = userID
	}
	page, err := h.svc.ListPhotos(r.Context(), tenantID, r.PathValue("id"), f, respond.PageParamsFromRequest(r))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]PhotoDTO, len(page.Items))
	for i := range page.Items {
		if out[i], err = h.svc.photoResponse(&page.Items[i]); err != nil {
			fail(w, err)
			return
		}
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[PhotoDTO]{Items: out, NextCursor: page.NextCursor})
}

// ==================== albums ====================

// ListAlbums handles GET /events/{id}/albums.
func (h *Handler) ListAlbums(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	albums, err := h.svc.ListAlbums(r.Context(), tenantID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]AlbumDTO, len(albums))
	for i := range albums {
		out[i] = albumResponse(&albums[i])
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[AlbumDTO]{Items: out})
}

// CreateAlbum handles POST /events/{id}/albums.
func (h *Handler) CreateAlbum(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req createAlbumRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	a, err := h.svc.CreateAlbum(r.Context(), tenantID, r.PathValue("id"), userID, role,
		AlbumInput{Name: req.Name, Description: req.Description})
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, albumResponse(a))
}

// UpdateAlbum handles PATCH /events/{id}/albums/{aid}.
func (h *Handler) UpdateAlbum(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req updateAlbumRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	a, err := h.svc.UpdateAlbum(r.Context(), tenantID, r.PathValue("id"), r.PathValue("aid"), userID, role,
		AlbumPatch{Name: req.Name, Description: req.Description, IsPublic: req.IsPublic})
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, albumResponse(a))
}

// DeleteAlbum handles DELETE /events/{id}/albums/{aid}.
func (h *Handler) DeleteAlbum(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	if err := h.svc.DeleteAlbum(r.Context(), tenantID, r.PathValue("id"), r.PathValue("aid"), userID, role); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
