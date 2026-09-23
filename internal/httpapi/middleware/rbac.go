package middleware

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
)

// RequireRole (role-hierarchy enforcement: Owner > Admin > Staff) lives in
// internal/tenant now, not here - role hierarchy is tenant's own
// invariant, and middleware never imports a bounded-context package. See
// tenant.RequireRole and docs/phase0-refactor-plan.md §4.1.

// RequireSuperAdmin gates Platform Super Admin-only endpoints (global,
// cross-tenant operations per the PRD's RBAC matrix). Must run after
// RequireUserAuth.
func RequireSuperAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !reqctx.IsSuperAdmin(r.Context()) {
			respond.Error(w, http.StatusForbidden, "forbidden", "This endpoint is restricted to platform administrators.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
