package oauthclient

// Scopes an OAuth 2.0 Client Credentials client can be granted, per the
// implementation guide's example ("events:read", "timing:write"). Later
// phases will extend this list as new resource APIs ship; Phase 0 itself
// only needs the mechanism plus a couple of representative scopes to prove
// the grant flow end to end.
//
// These constants live here rather than in internal/domain because scope
// validity is oauthclient's own invariant ("a client can only be granted
// scopes that exist") - every other bounded context that gates an M2M
// route by scope (today: the tenant resource-summary M2M variant; later:
// Phase 1's event/participant modules) imports oauthclient downward for
// the string constant only, a harmless one-directional edge since
// oauthclient never imports any of them back.
const (
	ScopeEventsRead        = "events:read"
	ScopeEventsWrite       = "events:write"
	ScopeRegistrationsRead = "registrations:read"
	ScopeTimingWrite       = "timing:write"
	ScopeTenantRead        = "tenant:read"
)

var validScopes = map[string]bool{
	ScopeEventsRead:        true,
	ScopeEventsWrite:       true,
	ScopeRegistrationsRead: true,
	ScopeTimingWrite:       true,
	ScopeTenantRead:        true,
}

func IsValidScope(scope string) bool {
	return validScopes[scope]
}
