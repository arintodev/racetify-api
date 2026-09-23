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
}

var validCapabilities = map[string]bool{
	CapabilityParticipantsRead: true,
	CapabilityRPCCheckin:       true,
	CapabilityResultsWrite:     true,
	CapabilityGalleryUpload:    true,
}

func IsValidCapability(capability string) bool {
	return validCapabilities[capability]
}
