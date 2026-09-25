// Package generator is the bounded context for the BIB / e-certificate
// generator's templates (docs/phase1-api-plan.md §4.3): the SVG a template is
// made of lives in Object Storage (storage_id -> objects), this package owns
// the row that says which event, race and service it is for, and which
// template applies where.
package generator

import (
	"encoding/json"
	"net/http"
	"time"
)

// Kind is what a template generates.
type Kind string

const (
	KindBIB         Kind = "bib"
	KindCertificate Kind = "certificate"
)

func (k Kind) Valid() bool { return k == KindBIB || k == KindCertificate }

// Template is one row of generator_templates plus the object it points at.
type Template struct {
	ID        string
	TenantID  string
	EventID   string
	RaceID    *string // nil: applies to every race of the event
	StorageID string
	Name      string
	Kind      Kind
	// IsActive: the template is the one picked automatically for its scope
	// (the race, or the event when RaceID is nil). At most one active
	// template per (event, kind, scope).
	IsActive bool
	// Version goes up each time the SVG is replaced.
	Version int
	// Metadata is free-form bookkeeping the dashboard keeps beside the SVG
	// (size, tags used); a JSON object.
	Metadata  json.RawMessage
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time

	// The object behind StorageID.
	Bucket      string
	ObjectKey   string
	ContentType string
	ObjectState string
}

// Error is a failure the client can act on: it carries its own HTTP status,
// a stable code, and the request field it concerns.
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
