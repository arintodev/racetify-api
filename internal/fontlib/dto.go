package fontlib

import (
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/generator"
)

// FontDTO is a font as every signed-in user sees it (the studio's list).
// Source is "library" or "bundled"; Styles are the ones that have a file.
type FontDTO struct {
	Key    string   `json:"key"`
	Family string   `json:"family"`
	Source string   `json:"source"`
	Status string   `json:"status"`
	Styles []string `json:"styles"`
	// FileURLs are paths (under /api/v1) to fetch each style's file with the session.
	Files map[string]string `json:"files"`
	// Hashes fingerprint each file, so the browser can keep what it has.
	Hashes map[string]string `json:"hashes,omitempty"`
}

func fontResponse(f Font) FontDTO {
	d := FontDTO{Key: f.Key(), Family: f.Family, Source: "library", Status: f.Status, Files: map[string]string{}, Hashes: map[string]string{}}
	for _, file := range f.Files {
		d.Styles = append(d.Styles, file.Style)
		d.Files[file.Style] = "fonts/files/" + f.ID + "/" + file.Style
		d.Hashes[file.Style] = file.SHA256
	}
	return d
}

func bundledResponse(f generator.BundledFamily) FontDTO {
	d := FontDTO{Key: f.Key, Family: f.Family, Source: "bundled", Status: StatusActive, Styles: f.Styles(), Files: map[string]string{}}
	for _, style := range d.Styles {
		d.Files[style] = "fonts/bundled/" + strings.TrimPrefix(f.Key, "bundled:") + "/" + style
	}
	return d
}

// AdminFileDTO is one style's file in the platform's font page.
type AdminFileDTO struct {
	Style      string `json:"style"`
	SizeBytes  int    `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	FSType     int    `json:"fs_type"`
	GlyphCount int    `json:"glyph_count"`
}

// AdminFontDTO is a library font with what the platform administrator needs.
type AdminFontDTO struct {
	ID          string         `json:"id"`
	Key         string         `json:"key"`
	Family      string         `json:"family"`
	Status      string         `json:"status"`
	LicenseNote string         `json:"license_note"`
	Files       []AdminFileDTO `json:"files"`
	// UsedBy is how many templates need this family.
	UsedBy    int       `json:"used_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func adminResponse(f Font, usedBy int) AdminFontDTO {
	d := AdminFontDTO{
		ID: f.ID, Key: f.Key(), Family: f.Family, Status: f.Status, LicenseNote: f.LicenseNote,
		Files: make([]AdminFileDTO, 0, len(f.Files)), UsedBy: usedBy, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
	}
	for _, file := range f.Files {
		d.Files = append(d.Files, AdminFileDTO{
			Style: file.Style, SizeBytes: file.SizeBytes, SHA256: file.SHA256, FSType: file.FSType, GlyphCount: file.GlyphCount,
		})
	}
	return d
}

// SavedDTO is a font after an upload, with the warnings the file earned.
type SavedDTO struct {
	Font     AdminFontDTO `json:"font"`
	Warnings []string     `json:"warnings"`
}

// TemplateUseDTO is a template that needs a font.
type TemplateUseDTO struct {
	TemplateID   string   `json:"template_id"`
	TemplateName string   `json:"template_name"`
	Service      string   `json:"service"`
	EventName    string   `json:"event_name"`
	TenantName   string   `json:"tenant_name"`
	Styles       []string `json:"styles"`
}

// DemandDTO is a font style templates ask for and the library lacks.
type DemandDTO struct {
	Family    string `json:"family"`
	Style     string `json:"style"`
	Templates int    `json:"templates"`
	Tenants   int    `json:"tenants"`
}

type patchRequest struct {
	Family      *string `json:"family"`
	Status      *string `json:"status"`
	LicenseNote *string `json:"license_note"`
}
