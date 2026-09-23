package tenant

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/mailer"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/security"
)

// Service implements "Tenant / Organizer Registration (Onboarding)" and
// "Tenant Staff Onboarding (Invitation Flow)" from the implementation
// guide.
//
// users *auth.Repository is a one-directional, downward dependency on the
// auth bounded context (auth never imports tenant back, so this is not a
// cycle) - looking up an inviter's display name in InviteStaff, and
// confirming an invitation acceptor's email matches in AcceptInvitation.
// Before docs/phase0-refactor-plan.md's Step 8 extracted internal/auth,
// this was a temporary, explicitly-flagged dependency on the legacy
// internal/repository.UserRepository/internal/domain.User; it is now the
// permanent, intended shape.
type Service struct {
	db     *database.DB
	repo   *Repository
	users  *auth.Repository
	audit  *audit.Repository
	mailer mailer.Mailer
	cfg    config.AuthConfig
}

func NewService(
	db *database.DB,
	repo *Repository,
	users *auth.Repository,
	audit *audit.Repository,
	m mailer.Mailer,
	cfg config.AuthConfig,
) *Service {
	return &Service{db: db, repo: repo, users: users, audit: audit, mailer: m, cfg: cfg}
}

var slugSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugSanitizer.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// normalizeEmail is duplicated from internal/service/auth_service.go
// (rather than shared) since it is a single, trivial, dependency-free
// string helper - not worth inventing a shared leaf package for two
// callers. Once internal/auth is extracted (Step 8), each bounded context
// keeps its own copy rather than one importing the other for it.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CreateTenant registers a new Tenant/Organizer workspace and makes
// ownerUserID its Tenant Owner in the same transaction, matching the
// guide's "Pengguna yang mendaftarkan tenant otomatis ditetapkan sebagai
// Tenant Owner". The tenant starts 'pending_verification': later phases
// gate certain capabilities (e.g. publishing an event) behind platform
// verification, per the PRD's Organizer Journey.
func (s *Service) CreateTenant(ctx context.Context, ownerUserID, name, slug string) (*Tenant, error) {
	if slug == "" {
		slug = Slugify(name)
	} else {
		slug = Slugify(slug)
	}
	if slug == "" {
		return nil, fmt.Errorf("service: %w: tenant name/slug must contain at least one alphanumeric character", domain.ErrInvalidState)
	}

	now := time.Now().UTC()
	tenant := &Tenant{
		ID:          security.MustNewUUIDv4(),
		Name:        name,
		Slug:        slug,
		OwnerUserID: ownerUserID,
		Status:      TenantStatusPendingVerification,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// The tenant's own ID is generated client-side (in Go) before the row
	// exists, so WithTenantTx can open its RLS context immediately; the
	// INSERT into tenants (unscoped, no RLS) and the INSERT into
	// tenant_members (RLS-protected, satisfied because the context is
	// already set to tenant.ID) both happen inside the same transaction.
	err := s.db.WithTenantTx(ctx, tenant.ID, func(ctx context.Context) error {
		if err := s.repo.CreateTenant(ctx, tenant); err != nil {
			return err
		}
		member := &TenantMember{
			ID:        security.MustNewUUIDv4(),
			TenantID:  tenant.ID,
			UserID:    ownerUserID,
			Role:      RoleOwner,
			Status:    MemberStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.repo.CreateMember(ctx, member); err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenant.ID, &ownerUserID, nil, audit.ActionTenantCreated, map[string]any{"slug": slug})
	})
	if err != nil {
		return nil, err
	}
	return tenant, nil
}

// ListTenantsForUser answers "which tenants (and with which role) does
// this user belong to" - see Repository.ListTenantsForUser's doc comment
// for why this is safe to answer without an existing tenant context.
func (s *Service) ListTenantsForUser(ctx context.Context, userID string) ([]TenantWithRole, error) {
	return s.repo.ListTenantsForUser(ctx, userID)
}

// InviteStaff implements the Tenant Staff Onboarding invitation flow.
// actorRole is the acting user's role within tenantID, already resolved
// and verified by middleware.RequireTenantForUser/RequireRole before the
// handler ever calls this method.
func (s *Service) InviteStaff(ctx context.Context, tenantID, actorUserID string, actorRole, targetRole MemberRole, email string) (*Invitation, string, error) {
	if !actorRole.IsAtLeast(RoleAdmin) {
		return nil, "", domain.ErrForbidden
	}
	if targetRole != RoleAdmin && targetRole != RoleStaff {
		return nil, "", fmt.Errorf("service: %w: role must be admin or staff", domain.ErrInvalidState)
	}
	if targetRole == RoleAdmin && actorRole != RoleOwner {
		// Only the Owner may grant Admin; an Admin may only invite Staff.
		return nil, "", domain.ErrForbidden
	}

	email = normalizeEmail(email)
	rawToken, err := security.GenerateOpaqueToken(24)
	if err != nil {
		return nil, "", err
	}

	inv := &Invitation{
		ID:        security.MustNewUUIDv4(),
		TenantID:  tenantID,
		Email:     email,
		Role:      targetRole,
		TokenHash: security.HashToken(rawToken),
		InvitedBy: actorUserID,
		Status:    InvitationStatusPending,
		ExpiresAt: time.Now().UTC().Add(s.cfg.InvitationTokenTTL),
		CreatedAt: time.Now().UTC(),
	}

	var tenant *Tenant
	var inviter *auth.User
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.CreateInvitation(ctx, inv); err != nil {
			return err
		}
		var err error
		tenant, err = s.repo.GetTenantByID(ctx, tenantID)
		if err != nil {
			return err
		}
		inviter, err = s.users.GetUserByID(ctx, actorUserID)
		if err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionInvitationCreated,
			map[string]any{"email": email, "role": string(targetRole)})
	})
	if err != nil {
		return nil, "", err
	}

	_ = s.mailer.SendInvitationEmail(ctx, email, tenant.Name, inviter.DisplayName(), invitationLink(rawToken))
	return inv, rawToken, nil
}

// ListInvitations returns one keyset-paginated page of tenantID's
// invitations - see internal/platform/pagination for why keyset
// rather than LIMIT/OFFSET.
func (s *Service) ListInvitations(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[Invitation], error) {
	var out pagination.Page[Invitation]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListInvitations(ctx, tenantID, page)
		return err
	})
	return out, err
}

func (s *Service) RevokeInvitation(ctx context.Context, tenantID, actorUserID string, actorRole MemberRole, invitationID string) error {
	if !actorRole.IsAtLeast(RoleAdmin) {
		return domain.ErrForbidden
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.RevokeInvitation(ctx, tenantID, invitationID); err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionInvitationRevoked, map[string]any{"invitation_id": invitationID})
	})
}

// AcceptInvitation implements the invitee's side of the Staff Onboarding
// flow. acceptingUserID must belong to an already-authenticated User whose
// account email matches the invitation (the invitee logs in or registers
// first, then redeems the invite link) - this keeps "who can join a
// tenant" tied to Racetify's single verified identity per the "Single
// Runner Identity" principle, rather than trusting the email address
// embedded in the link alone.
func (s *Service) AcceptInvitation(ctx context.Context, rawToken, acceptingUserID string) (*TenantMember, error) {
	inv, err := s.repo.GetInvitationByTokenHash(ctx, security.HashToken(rawToken))
	if err != nil {
		return nil, err
	}
	if inv.Status != InvitationStatusPending {
		return nil, domain.ErrInvalidState
	}
	if inv.IsExpired(time.Now().UTC()) {
		return nil, domain.ErrTokenExpired
	}

	user, err := s.users.GetUserByID(ctx, acceptingUserID)
	if err != nil {
		return nil, err
	}
	if normalizeEmail(user.Email) != normalizeEmail(inv.Email) {
		return nil, fmt.Errorf("service: %w: invitation was issued to a different email address", domain.ErrForbidden)
	}

	var member *TenantMember
	err = s.db.WithTenantTx(ctx, inv.TenantID, func(ctx context.Context) error {
		if err := s.repo.MarkInvitationAccepted(ctx, inv.ID, time.Now().UTC()); err != nil {
			return err
		}
		now := time.Now().UTC()
		member = &TenantMember{
			ID:        security.MustNewUUIDv4(),
			TenantID:  inv.TenantID,
			UserID:    acceptingUserID,
			Role:      inv.Role,
			Status:    MemberStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.repo.CreateMember(ctx, member); err != nil {
			return err
		}
		return s.recordAudit(ctx, &inv.TenantID, &acceptingUserID, nil, audit.ActionInvitationAccepted, map[string]any{"invitation_id": inv.ID})
	})
	if err != nil {
		return nil, err
	}
	return member, nil
}

// ListMembers returns one keyset-paginated page of tenantID's members -
// see internal/platform/pagination for why keyset rather than
// LIMIT/OFFSET.
func (s *Service) ListMembers(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[TenantMember], error) {
	var out pagination.Page[TenantMember]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListMembers(ctx, tenantID, page)
		return err
	})
	return out, err
}

// TenantSummary is the payload behind GET /api/v1/tenant/summary - a
// deliberately minimal protected resource reachable by both a user
// session and an M2M token (scope tenant:read), used to demonstrate end
// to end that a caller only ever sees ITS OWN tenant's data: the
// oauth-client count comes from the tenant_members-sibling oauth_clients
// table, which is RLS-protected exactly like tenant_members itself (see
// TestTenantIsolation in internal/repository for the same guarantee
// exercised directly at the SQL level).
type TenantSummary struct {
	TenantID         string
	TenantName       string
	OAuthClientCount int
}

func (s *Service) GetTenantSummary(ctx context.Context, tenantID string) (*TenantSummary, error) {
	var summary *TenantSummary
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		tenant, err := s.repo.GetTenantByID(ctx, tenantID)
		if err != nil {
			return err
		}
		var count int
		row := s.db.Q(ctx).QueryRowContext(ctx, `SELECT count(*) FROM oauth_clients WHERE tenant_id = $1`, tenantID)
		if err := row.Scan(&count); err != nil {
			return err
		}
		summary = &TenantSummary{TenantID: tenant.ID, TenantName: tenant.Name, OAuthClientCount: count}
		return nil
	})
	return summary, err
}

// SetTenantStatus implements the Platform Super Admin's "Platform
// Verification" capability from the PRD ("verifikasi organizer"): approving
// (Active), rejecting, or suspending a tenant, with a required reason for
// the latter two - see Tenant.StatusReason's doc comment for why that
// reason exists (so a Tenant Owner can be shown *why* without needing
// audit-log access). actorIsSuperAdmin is already checked by
// middleware.RequireSuperAdmin at the router; it is re-checked here as the
// real enforcement point, the same double-gating pattern InviteStaff/
// RevokeInvitation use for actorRole.
//
// This runs inside WithTenantTx (not a plain db.Q write) purely so the
// audit_logs insert - tenant-scoped, from a platform actor who has no
// membership in this tenant - satisfies audit_logs' RLS WITH CHECK, which
// requires app.tenant_id to equal the row's tenant_id (see
// 0003_row_level_security.up.sql). tenants itself has no RLS (Repository's
// tenants-section doc comment), so UpdateTenantStatus/GetTenantByID would
// work over a plain connection; they just ride along in the same
// transaction as the audit write.
func (s *Service) SetTenantStatus(ctx context.Context, actorUserID string, actorIsSuperAdmin bool, tenantID string, status TenantStatus, reason *string) (*Tenant, error) {
	if !actorIsSuperAdmin {
		return nil, domain.ErrForbidden
	}
	switch status {
	case TenantStatusActive, TenantStatusRejected, TenantStatusSuspended:
	default:
		return nil, fmt.Errorf("service: %w: status must be one of active, rejected, suspended", domain.ErrInvalidState)
	}
	if status == TenantStatusActive {
		reason = nil // clear any prior rejection/suspension reason on reactivation
	} else if reason == nil || strings.TrimSpace(*reason) == "" {
		return nil, fmt.Errorf("service: %w: reason is required when rejecting or suspending a tenant", domain.ErrInvalidState)
	}

	var tenant *Tenant
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.UpdateTenantStatus(ctx, tenantID, status, reason); err != nil {
			return err
		}
		var err error
		tenant, err = s.repo.GetTenantByID(ctx, tenantID)
		if err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionTenantStatusChanged,
			map[string]any{"status": string(status), "reason": reason})
	})
	if err != nil {
		return nil, err
	}
	return tenant, nil
}

func (s *Service) recordAudit(ctx context.Context, tenantID, actorUserID, actorClientID *string, action string, metadata map[string]any) error {
	return s.audit.Record(ctx, &audit.Log{
		ID:            security.MustNewUUIDv4(),
		TenantID:      tenantID,
		ActorUserID:   actorUserID,
		ActorClientID: actorClientID,
		Action:        action,
		Metadata:      metadata,
		CreatedAt:     time.Now().UTC(),
	})
}

func invitationLink(token string) string {
	return fmt.Sprintf("https://app.racetify.id/invitations/accept?token=%s", token)
}
