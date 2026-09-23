package event

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// RequireEventAccess gates an event-scoped route (event_id in the URL,
// pattern {id}) to either an existing tenant session with role >= Staff
// for that event's tenant (the internal-staff path, identical to every
// other event-scoped route's gate), or an active EventAssignment carrying
// the given capability (the external crew/volunteer path,
// docs/event-crew-access-plan.md §4). Pass capability = "" to accept any
// active assignment regardless of which capabilities it carries (used by
// the "what am I assigned to do here" self-check endpoint).
//
// It must run right after mw.RequireUserAuth ONLY - deliberately NOT
// mw.RequireTenantForUser, since deriving tenant_id from the event_id
// path param is exactly its own job, and mw.RequireTenantForUser would
// hard-403 the exact caller Path B below exists for (someone with no
// tenant_members row in this tenant at all). This is the same "resolve
// tenant from a foreign identifier, outside any established tenant
// context" bootstrap shape phase1-api-plan.md §5 already documents for
// the Runner Portal's planned ResolvePublicEvent - plus a membership
// check that middleware doesn't need, because there is no principal there
// at all.
func (s *Service) RequireEventAccess(capability string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := reqctx.UserID(r.Context())
			if !ok {
				respond.Error(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
				return
			}
			eventID := r.PathValue("id")

			tenantID, err := s.repo.TenantIDForEvent(r.Context(), eventID)
			if err != nil {
				respond.Error(w, http.StatusNotFound, "not_found", "Event not found.")
				return
			}

			// Path A: caller's access token carries a tid claim for this
			// event's tenant - re-verify role fresh from the database
			// rather than trusting the claim as-is, the same guarantee
			// mw.RequireTenantForUser gives every other tenant-scoped
			// route (a membership revoked after the token was minted is
			// caught immediately here, not just trusted from the JWT).
			if claimTenantID, ok := reqctx.TenantID(r.Context()); ok && claimTenantID == tenantID {
				var role string
				var active bool
				err := s.db.WithTenantTx(r.Context(), tenantID, func(ctx context.Context) error {
					var err error
					role, active, err = s.members.ActiveRole(ctx, tenantID, userID)
					return err
				})
				if err != nil {
					respond.Error(w, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
					return
				}
				if active && rbac.MemberRole(role).IsAtLeast(rbac.RoleStaff) {
					ctx := reqctx.WithTenantID(r.Context(), tenantID)
					ctx = reqctx.WithMemberRole(ctx, role)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			// Path B: no qualifying tenant session - the only path an
			// externally-assigned crew/volunteer ever takes; auth.Service.
			// Login always issues a tenant-less access token by default
			// (see its own doc comment), so they never acquire a tid
			// claim at all unless they also separately are a tenant
			// member somewhere.
			var assignment *EventAssignment
			err = s.db.WithTenantTx(r.Context(), tenantID, func(ctx context.Context) error {
				var err error
				assignment, err = s.repo.GetAssignmentForUser(ctx, tenantID, eventID, userID)
				return err
			})
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				respond.Error(w, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
				return
			}
			if assignment == nil || !assignment.IsActive(time.Now().UTC()) ||
				(capability != "" && !assignment.HasCapability(capability)) {
				respond.Error(w, http.StatusForbidden, "forbidden", "You are not assigned to this event.")
				return
			}

			ctx := reqctx.WithTenantID(r.Context(), tenantID)
			ctx = reqctx.WithEventCapabilities(ctx, assignment.Capabilities)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
