// Package rbac holds the tenant-membership role hierarchy (Owner > Admin >
// Staff) as a small, dependency-free type - the same "genuinely shared
// vocabulary, no single owning bounded context" role already played by
// internal/platform/pagination and internal/platform/dbutil.
//
// internal/tenant owns the tenant_members table and this role's *meaning*
// (who is allowed to invite staff, revoke access, etc.), so at first glance
// MemberRole looks like it should just live in internal/tenant. It can't:
// internal/oauthclient and internal/storage both gate actions on
// "is this actor's role at least admin" before provisioning an OAuth
// client or a presigned upload URL, which means they need the type too -
// and internal/tenant/routes.go already imports internal/oauthclient
// downward, for its ScopeTenantRead constant (see oauthclient/scope.go's
// doc comment). internal/oauthclient importing internal/tenant back for
// MemberRole would close that into an import cycle. Splitting the role
// type out to this leaf package breaks the cycle while keeping one
// definition; internal/tenant re-exports it as a type alias (see
// tenant.go) so nothing under internal/tenant had to change to use it.
package rbac

// MemberRole is a tenant-scoped role, distinct from the platform-level
// IsSuperAdmin flag on the auth bounded context's User.
type MemberRole string

const (
	RoleOwner MemberRole = "owner"
	RoleAdmin MemberRole = "admin"
	RoleStaff MemberRole = "staff"
)

// IsAtLeast reports whether r grants at least the privilege of min, using
// the strict hierarchy owner > admin > staff described in the PRD's RBAC
// matrix.
func (r MemberRole) IsAtLeast(min MemberRole) bool {
	rank := map[MemberRole]int{RoleStaff: 1, RoleAdmin: 2, RoleOwner: 3}
	return rank[r] >= rank[min]
}
