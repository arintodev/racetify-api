// Package certificate is the bounded context for e-certificates
// (docs/e-certificate-ux.md): one image file per participant. The dashboard
// renders the image and uploads it to Object Storage; this package owns the
// row that says which participant it is for, which template version and data
// it was made from (so the dashboard can tell it is out of date), and whether
// the event's certificates are published.
package certificate

import (
	"net/http"
	"time"
)

// Status of a certificate record.
const (
	StatusReady  = "ready"
	StatusFailed = "failed"
)

// Certificate is one row of certificates plus the object behind StorageID.
type Certificate struct {
	ID              string
	TenantID        string
	EventID         string
	ParticipantID   string
	CertificateNo   string
	TemplateID      *string
	TemplateVersion *int
	DataHash        *string
	Status          string
	Error           *string
	StorageID       *string
	Format          *string
	DPI             *int
	WidthPx         *int
	HeightPx        *int
	SizeBytes       *int64
	GeneratedAt     *time.Time
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time

	// The object behind StorageID; empty without one.
	Bucket    string
	ObjectKey string
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
