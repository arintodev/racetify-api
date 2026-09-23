// Package tenant is the bounded context for the Tenant/Organizer
// workspace: the tenants and tenant_members tables, staff invitations
// (invitation.go), RBAC role-hierarchy enforcement (rbac.go), and the
// Phase 0 demo resource endpoint that proves tenant isolation across both
// auth models (folded into handler.go/routes.go - see
// docs/phase0-refactor-plan.md §6). It is distinct from "multi-tenancy"
// the cross-cutting mechanism (tenant_id columns, RLS, WithTenantTx),
// which stays in internal/platform/database and is used *by* every
// bounded context, not owned by this package - see
// docs/phase0-refactor-plan.md §2's note on this non-collision.
package tenant

import (
	"time"

	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// TenantStatus is a plain string, not a DB-level enum/CHECK constraint -
// see migrations/0001_core.up.sql's doc comment for why.
type TenantStatus string

const (
	TenantStatusPendingVerification TenantStatus = "pending_verification"
	TenantStatusActive              TenantStatus = "active"
	TenantStatusRejected            TenantStatus = "rejected"
	TenantStatusSuspended           TenantStatus = "suspended"
)

// Tenant is an Event Organizer workspace. All operational data in later
// phases (events, races, participants, ...) hangs off tenant_id.
type Tenant struct {
	ID          string
	Name        string
	Slug        string
	OwnerUserID string
	Status      TenantStatus
	// StatusReason explains a Rejected or Suspended status - who did what
	// to which tenant is already in audit_logs (audit.ActionTenantStatusChanged),
	// but this is the one place a Tenant Owner can be shown *why*, without
	// needing audit-log access. Always nil for PendingVerification/Active;
	// set by Service.SetTenantStatus, cleared on reactivation.
	StatusReason *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// MemberRole is a tenant-scoped role, distinct from the platform-level
// IsSuperAdmin flag on the auth bounded context's User. It is a type alias
// (not a new named type) for internal/platform/rbac.MemberRole - see that
// package's doc comment for why the definition had to move out of here:
// internal/oauthclient and internal/storage both need to check role
// privilege too, and internal/tenant already imports internal/oauthclient
// downward (routes.go, for ScopeTenantRead), so the type can't live here
// without creating an import cycle. The alias means every existing
// reference in this package (MemberRole, RoleOwner/RoleAdmin/RoleStaff,
// r.IsAtLeast(...)) keeps compiling unchanged.
type MemberRole = rbac.MemberRole

const (
	RoleOwner = rbac.RoleOwner
	RoleAdmin = rbac.RoleAdmin
	RoleStaff = rbac.RoleStaff
)

type MemberStatus string

const (
	MemberStatusActive  MemberStatus = "active"
	MemberStatusRemoved MemberStatus = "removed"
)

// TenantMember links a User into a Tenant's workspace with a role. This is
// the table Row-Level Security protects: a request scoped to Tenant A must
// never see Tenant B's membership rows.
type TenantMember struct {
	ID        string
	TenantID  string
	UserID    string
	Role      MemberRole
	Status    MemberStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}
