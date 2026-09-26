// Package fontlib is the platform's font library: the fonts BIB and certificate
// templates may use. Platform administrators upload them as they are needed (a
// family may hold only the styles wanted, e.g. just Roboto Bold); every
// workspace uses them. The dashboard's studio shows which fonts a template
// needs and warns about those the library does not have, and the print jobs
// refuse a template whose fonts (or styles) are missing rather than drawing
// them in another font.
//
// Fonts are referred to by key: "lib:<id>" for a library family, "bundled:<name>"
// for the fonts built into the binary (internal/generator).
package fontlib

import (
	"net/http"
	"time"
)

// Status of a library font.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// KeyPrefix is the start of a library font's key.
const KeyPrefix = "lib:"

// FileInfo describes one style's file of a font (not its bytes).
type FileInfo struct {
	Style      string
	SizeBytes  int
	SHA256     string
	FSType     int
	GlyphCount int
}

// Font is a library family and the styles it has.
type Font struct {
	ID          string
	Family      string
	Status      string
	LicenseNote string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Files       []FileInfo
}

// Key is the font's key in templates.
func (f Font) Key() string { return KeyPrefix + f.ID }

// HasStyle reports whether the family has a file for the style.
func (f Font) HasStyle(style string) bool {
	for _, file := range f.Files {
		if file.Style == style {
			return true
		}
	}
	return false
}

// Error is a failure the client can act on.
type Error struct {
	Status  int
	Code    string
	Field   string
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(field, message string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Field: field, Message: message}
}

func conflict(code, field, message string) *Error {
	return &Error{Status: http.StatusConflict, Code: code, Field: field, Message: message}
}
