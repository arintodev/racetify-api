package portal

import (
	"errors"
	"net/http"
	"time"

	"github.com/racetify/racetify-api/internal/httpapi/respond"
)

// ListAlbums handles GET /api/v1/public/events/{slug}/gallery/albums.
func (h *Handler) ListAlbums(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListAlbums(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[AlbumDTO]{Items: items})
}

// SearchPhotos handles GET /api/v1/public/events/{slug}/gallery/photos?bib=.
func (h *Handler) SearchPhotos(w http.ResponseWriter, r *http.Request) {
	page, err := h.svc.SearchPhotos(r.Context(), r.PathValue("slug"), r.URL.Query().Get("bib"), respond.PageParamsFromRequest(r))
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[PhotoDTO]{Items: page.Items, NextCursor: page.NextCursor})
}

// ListAlbumPhotos handles GET /api/v1/public/events/{slug}/gallery/albums/{aid}/photos.
func (h *Handler) ListAlbumPhotos(w http.ResponseWriter, r *http.Request) {
	page, err := h.svc.ListAlbumPhotos(r.Context(), r.PathValue("slug"), r.PathValue("aid"), respond.PageParamsFromRequest(r))
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[PhotoDTO]{Items: page.Items, NextCursor: page.NextCursor})
}

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// GetEvent handles GET /api/v1/public/events/{slug}.
func (h *Handler) GetEvent(w http.ResponseWriter, r *http.Request) {
	dto, err := h.svc.GetEvent(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, dto)
}

// SearchCertificates handles GET /api/v1/public/events/{slug}/certificates/search?q=.
func (h *Handler) SearchCertificates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	items, err := h.svc.SearchCertificates(r.Context(), r.PathValue("slug"), q)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, searchResponse{Items: items})
}

// DownloadCertificate handles GET /api/v1/public/events/{slug}/certificates/{pid}/download.
func (h *Handler) DownloadCertificate(w http.ResponseWriter, r *http.Request) {
	url, expiresAt, err := h.svc.GetCertificateDownload(r.Context(), r.PathValue("slug"), r.PathValue("pid"))
	if err != nil {
		fail(w, err)
		return
	}
	resp := downloadResponse{FileURL: url}
	if expiresAt != nil {
		s := expiresAt.Format(time.RFC3339)
		resp.ExpiresAt = &s
	}
	respond.JSON(w, http.StatusOK, resp)
}

func fail(w http.ResponseWriter, err error) {
	var portalErr *Error
	if errors.As(err, &portalErr) {
		respond.Error(w, portalErr.Status, portalErr.Code, portalErr.Message)
		return
	}
	respond.FromServiceError(w, err)
}
