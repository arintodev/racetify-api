package middleware

import (
	"context"
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/database"
)

// MembershipChecker is the minimal membership-lookup surface
// RequireTenantForUser needs, declared here rather than importing
// internal/tenant directly - the same pattern RequireUserAuth already
// uses for its isRevoked callback (see that function's doc comment: "to
// avoid this package importing service - service stays a strict 'no
// net/http' layer"). internal/tenant's Repository satisfies this
// interface structurally via its ActiveRole method; middleware never
// imports internal/tenant, so a bounded-context package can freely import
// middleware's constructors to guard its own routes without an import
// cycle. See docs/phase0-refactor-plan.md §4.2.
type MembershipChecker interface {
	ActiveRole(ctx context.Context, tenantID, userID string) (role string, active bool, err error)
}

// RequireTenantForUser implements Tenant Resolution for a user-session
// (JWT) caller. It must run after RequireUserAuth in the chain (route
// definitions list middleware innermost-first - see routing.Chain's doc
// comment - so RequireUserAuth is the last one applied, meaning the first
// one to actually execute): that's what seeds ctx's tenant id from the
// access token's own `tid` claim, i.e. the tenant the caller selected at
// Login or via POST /auth/switch-tenant (auth.Service.SelectTenant) -
// there is no X-Tenant-ID header or other per-request signal anymore.
// This middleware re-verifies that selection from *inside* a short-lived
// WithTenantTx transaction, so the RLS policy on tenant_members is what
// actually decides whether the row is visible, not application logic
// alone - a membership revoked after the token was minted is caught
// immediately here rather than waiting for the token to expire, exactly
// the freshness guarantee the header-based version had. Once confirmed,
// tenantID and the caller's role are (re-)stashed into context for RBAC
// (internal/tenant's RequireRole) and handlers; the transaction used for
// the membership check itself is already committed and closed by the
// time next.ServeHTTP runs - every downstream repository call opens its
// own short tenant-scoped transaction via the same WithTenantTx
// mechanism.
func RequireTenantForUser(db *database.DB, members MembershipChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := reqctx.UserID(r.Context())
			if !ok {
				respond.Error(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
				return
			}
			tenantID, ok := reqctx.TenantID(r.Context())
			if !ok || tenantID == "" {
				respond.Error(w, http.StatusBadRequest, "tenant_required", "This endpoint requires a tenant-scoped access token. Call POST /api/v1/auth/switch-tenant to select one.")
				return
			}

			var role string
			var active bool
			err := db.WithTenantTx(r.Context(), tenantID, func(ctx context.Context) error {
				var err error
				role, active, err = members.ActiveRole(ctx, tenantID, userID)
				return err
			})
			if err != nil {
				respond.Error(w, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
				return
			}
			if !active {
				respond.Error(w, http.StatusForbidden, "forbidden", "You are not an active member of this tenant.")
				return
			}

			ctx := reqctx.WithTenantID(r.Context(), tenantID)
			ctx = reqctx.WithMemberRole(ctx, role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireTenantForM2M resolves tenant context for an M2M (client_
// credentials) caller directly from its already-verified JWT claims (see
// RequireM2MAuth) - no database lookup is needed because the tenant_id was
// burned into the token at grant time from the oauth_clients row itself,
// and cannot be forged without the signing secret. This mirrors the
// guide's "mengekstrak tenant_id dari token otorisasi (JWT User/M2M)"
// clause for the M2M case specifically.
func RequireTenantForM2M(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := reqctx.TenantID(r.Context()); !ok {
			respond.Error(w, http.StatusUnauthorized, "unauthorized", "M2M authentication required.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
