// Package portal is the unauthenticated Runner Portal
// (docs/phase1-api-plan.md §5): the public microsite a runner reaches at
// GET /api/v1/public/events/{slug}/... with no login at all. This first
// pass covers event info and e-certificate search/download
// (docs/e-certificate-ux.md Screen D); gallery search follows the same
// shape later.
//
// Every method resolves the event slug to a tenant_id first (Repository.
// event, on the BYPASSRLS admin connection - the same "resolve tenant
// from a foreign identifier, outside any established tenant context"
// bootstrap internal/event/access.go's RequireEventAccess already
// documents), then does the actual read inside database.DB.WithTenantTx
// like every other bounded context, so RLS still applies underneath.
package portal

import "net/http"

// CertificateStatus values a search result can carry - mirrors
// internal/certificate.StatusReady, kept as its own string here so this
// package does not need to import internal/certificate just for a
// constant.
const CertificateStatusReady = "ready"

// CertificateMatch is one participant a search matched, with just enough
// to disambiguate and to request the download - no result/time data,
// which lives in the certificate image itself.
type CertificateMatch struct {
	ParticipantID string
	BibNumber     int
	FullName      string
	RaceName      string
	Status        string
}

// Error is a failure the client can act on: its own HTTP status, a
// stable code, no field (portal has no form to point at, only lookups).
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func notFound(message string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "not_found", Message: message}
}

func invalid(message string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: message}
}
