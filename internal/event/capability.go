package event

// Capabilities an EventAssignment (or a pending EventInvitation) can
// grant, scoped to exactly one event (docs/event-crew-access-plan.md
// §2.4) - the vocabulary a RequireEventAccess-gated route checks a caller
// against. Plain untyped string constants, the same shape as
// internal/oauthclient's Scope* constants and for the same reason: every
// bounded context that will eventually gate a route by one of these (the
// RPC check-in module, once it exists) only needs the string, never this
// package's other internals.
const (
	CapabilityParticipantsRead = "participants:read"
	CapabilityRPCCheckin       = "rpc:checkin"
	CapabilityResultsWrite     = "results:write"
	CapabilityGalleryUpload    = "gallery:upload"
	// CapabilityGalleryReview allows manual tagging and re-running OCR on an
	// event's photos, but not deleting them or publishing albums.
	CapabilityGalleryReview = "gallery:review"
	// CapabilityGalleryFaceSearch allows enrolling a user's face and
	// searching an event's gallery photos by an enrolled face
	// (docs/face-search-plan.md, racetify-app repo). Separate from
	// CapabilityGalleryReview so it can be granted independently.
	CapabilityGalleryFaceSearch = "gallery:face_search"
)

// AllCapabilities is what an internal Staff/Admin/Owner caller effectively
// has on every event in their tenant already (every event-scoped route's
// existing Staff+ gate) - used by the "what can I do here" self-check
// handler (assignment_handler.go's MyAssignmentOnEvent) to report that
// blanket access in the same shape an externally-assigned crew member's
// actual capability list is reported in.
var AllCapabilities = []string{
	CapabilityParticipantsRead,
	CapabilityRPCCheckin,
	CapabilityResultsWrite,
	CapabilityGalleryUpload,
	CapabilityGalleryReview,
	CapabilityGalleryFaceSearch,
}

var validCapabilities = map[string]bool{
	CapabilityParticipantsRead:  true,
	CapabilityRPCCheckin:        true,
	CapabilityResultsWrite:      true,
	CapabilityGalleryUpload:     true,
	CapabilityGalleryReview:     true,
	CapabilityGalleryFaceSearch: true,
}

func IsValidCapability(capability string) bool {
	return validCapabilities[capability]
}
