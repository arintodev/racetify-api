package tenant

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
)

// RequireRole enforces the PRD's tenant RBAC matrix (Owner > Admin >
// Staff) for a user-session route. It must run after
// middleware.RequireTenantForUser, which is what populates the member-role
// context value.
//
// This lives here rather than in internal/httpapi/middleware because
// role-hierarchy comparison (owner > admin > staff) is tenant's own
// invariant, exactly like bib_number uniqueness will be participant's in
// Phase 1 - see docs/phase0-refactor-plan.md §4.1. middleware itself never
// imports this package; router.go calls tenant.RequireRole directly when
// building its routing.Middlewares.
func RequireRole(min MemberRole) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			roleStr, ok := reqctx.MemberRole(r.Context())
			if !ok {
				respond.Error(w, http.StatusForbidden, "forbidden", "Tenant context is required for this endpoint.")
				return
			}
			if !MemberRole(roleStr).IsAtLeast(min) {
				respond.Error(w, http.StatusForbidden, "forbidden", "Your role does not permit this action.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
