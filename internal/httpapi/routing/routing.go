// Package routing holds the shared middleware-chain type and helper that
// every route-registration file depends on, without needing to import
// internal/httpapi itself. Extracted out of router.go so bounded-context
// packages (internal/auth, internal/tenant, internal/oauthclient,
// internal/storage, and Phase 1's internal/event, internal/participant,
// ...) can each register their own routes with the same guard set
// router.go builds once, without router.go and those packages ending up
// in a two-way import (Go rejects that): both sides depend downward on
// this neutral leaf instead.
package routing

import "net/http"

// Middlewares is the shared set of route-guard middleware, built once per
// httpapi.NewRouter call and handed to every bounded context's
// RegisterRoutes function, so "which middleware guards this route" always
// reads the same way no matter which package the route lives in.
type Middlewares struct {
	RequireUserAuth      func(http.Handler) http.Handler
	RequireM2MAuth       func(http.Handler) http.Handler
	RequireTenantForUser func(http.Handler) http.Handler
	RequireAdminRole     func(http.Handler) http.Handler
	RequireAnyRole       func(http.Handler) http.Handler
	RequireSuperAdmin    func(http.Handler) http.Handler
	LoginRateLimit       func(http.Handler) http.Handler
	SignupRateLimit      func(http.Handler) http.Handler
	TokenRateLimit       func(http.Handler) http.Handler
}

// Chain applies middleware in the order listed - the first argument after
// the handler wraps closest, so `Chain(h, requireRole, requireTenant,
// requireAuth)` executes requireAuth first, then requireTenant, then
// requireRole, then h, matching the visual top-to-bottom order route
// definitions read in below.
func Chain(h http.HandlerFunc, mws ...func(http.Handler) http.Handler) http.Handler {
	var handler http.Handler = h
	for _, mw := range mws {
		handler = mw(handler)
	}
	return handler
}
