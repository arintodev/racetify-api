package fontlib

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
)

// maxUploadBytes caps an upload: four files at most, plus form fields.
const maxUploadBytes = 4*MaxFontBytes + (1 << 20)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func fail(w http.ResponseWriter, err error) {
	var ferr *Error
	if errors.As(err, &ferr) {
		respond.ErrorWithField(w, ferr.Status, ferr.Code, ferr.Message, ferr.Field)
		return
	}
	respond.FromServiceError(w, err)
}

func actor(r *http.Request) (userID string, isSuperAdmin bool) {
	userID, _ = reqctx.UserID(r.Context())
	return userID, reqctx.IsSuperAdmin(r.Context())
}

// ---- for every signed-in user ----

// List handles GET /fonts: the built-in fonts and the library's.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	fonts, err := h.svc.List(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	items := make([]FontDTO, 0, len(fonts)+len(generator.BundledFamilies()))
	for _, f := range generator.BundledFamilies() {
		items = append(items, bundledResponse(f))
	}
	for _, f := range fonts {
		items = append(items, fontResponse(f))
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[FontDTO]{Items: items})
}

// serveFont writes a font file; fonts never change under one hash, so the
// browser keeps them.
func serveFont(w http.ResponseWriter, r *http.Request, data []byte, sha string) {
	etag := `"` + sha + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "font/ttf")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// File handles GET /fonts/files/{id}/{style}.
func (h *Handler) File(w http.ResponseWriter, r *http.Request) {
	file, err := h.svc.File(r.Context(), r.PathValue("id"), r.PathValue("style"))
	if err != nil {
		fail(w, err)
		return
	}
	serveFont(w, r, file.Data, file.SHA256)
}

// BundledFile handles GET /fonts/bundled/{name}/{style}.
func (h *Handler) BundledFile(w http.ResponseWriter, r *http.Request) {
	family, ok := generator.BundledFamilyByKey("bundled:" + r.PathValue("name"))
	if !ok {
		respond.Error(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	data, ok := family.File(r.PathValue("style"))
	if !ok {
		respond.Error(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	serveFont(w, r, data, digest(data))
}

// ---- platform administrators ----

// AdminList handles GET /admin/fonts.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	_, super := actor(r)
	listing, err := h.svc.AdminList(r.Context(), super)
	if err != nil {
		fail(w, err)
		return
	}
	items := make([]AdminFontDTO, len(listing.Fonts))
	for i, f := range listing.Fonts {
		items[i] = adminResponse(f, listing.UseCounts[f.Key()])
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[AdminFontDTO]{Items: items})
}

func readFile(r *http.Request, field string) ([]byte, bool, error) {
	f, _, err := r.FormFile(field)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFontBytes+1))
	return data, true, err
}

// Create handles POST /admin/fonts (multipart: family, license_note, and a file
// per style: regular, bold, italic, bold_italic).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, super := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "send the font as multipart/form-data (each file at most 5 MB).")
		return
	}
	in := CreateInput{Family: r.FormValue("family"), LicenseNote: r.FormValue("license_note")}
	for _, style := range generator.StyleNames {
		data, ok, err := readFile(r, style)
		if err != nil {
			respond.Error(w, http.StatusBadRequest, "invalid_request", "the "+style+" file could not be read.")
			return
		}
		if ok {
			in.Files = append(in.Files, NewFile{Style: style, Data: data})
		}
	}
	font, warnings, err := h.svc.Create(r.Context(), userID, super, in)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, SavedDTO{Font: adminResponse(*font, 0), Warnings: nonNil(warnings)})
}

// Patch handles PATCH /admin/fonts/{id}.
func (h *Handler) Patch(w http.ResponseWriter, r *http.Request) {
	userID, super := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req patchRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	font, err := h.svc.Update(r.Context(), userID, super, r.PathValue("id"), PatchInput{
		Family: req.Family, Status: req.Status, LicenseNote: req.LicenseNote,
	})
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, adminResponse(*font, 0))
}

// Delete handles DELETE /admin/fonts/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, super := actor(r)
	if err := h.svc.Delete(r.Context(), userID, super, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// PutFile handles PUT /admin/fonts/{id}/files/{style}: the file is the form
// field "file", or the whole request body.
func (h *Handler) PutFile(w http.ResponseWriter, r *http.Request) {
	userID, super := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	var data []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			respond.Error(w, http.StatusBadRequest, "invalid_request", "the file could not be read (at most 5 MB).")
			return
		}
		var ok bool
		var err error
		if data, ok, err = readFile(r, "file"); err != nil || !ok {
			respond.Error(w, http.StatusBadRequest, "invalid_request", "send the font as the form field \"file\".")
			return
		}
	} else {
		var err error
		if data, err = io.ReadAll(io.LimitReader(r.Body, MaxFontBytes+1)); err != nil {
			respond.Error(w, http.StatusBadRequest, "invalid_request", "the file could not be read.")
			return
		}
	}
	font, warnings, err := h.svc.PutFile(r.Context(), userID, super, r.PathValue("id"), r.PathValue("style"), data)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, SavedDTO{Font: adminResponse(*font, 0), Warnings: nonNil(warnings)})
}

// RemoveFile handles DELETE /admin/fonts/{id}/files/{style}.
func (h *Handler) RemoveFile(w http.ResponseWriter, r *http.Request) {
	userID, super := actor(r)
	font, err := h.svc.RemoveFile(r.Context(), userID, super, r.PathValue("id"), r.PathValue("style"))
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, adminResponse(*font, 0))
}

// Uses handles GET /admin/fonts/{id}/usage.
func (h *Handler) Uses(w http.ResponseWriter, r *http.Request) {
	_, super := actor(r)
	uses, err := h.svc.Uses(r.Context(), super, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	items := make([]TemplateUseDTO, len(uses))
	for i, u := range uses {
		items[i] = TemplateUseDTO{
			TemplateID: u.TemplateID, TemplateName: u.TemplateName, Service: u.Service,
			EventName: u.EventName, TenantName: u.TenantName, Styles: nonNil(u.Styles),
		}
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[TemplateUseDTO]{Items: items})
}

// Demand handles GET /admin/fonts/demand.
func (h *Handler) Demand(w http.ResponseWriter, r *http.Request) {
	_, super := actor(r)
	rows, err := h.svc.Demand(r.Context(), super)
	if err != nil {
		fail(w, err)
		return
	}
	items := make([]DemandDTO, len(rows))
	for i, d := range rows {
		items[i] = DemandDTO{Family: d.Family, Style: d.Style, Templates: d.Templates, Tenants: d.Tenants}
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[DemandDTO]{Items: items})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
