package tenant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// Repository owns all three tables this bounded context is responsible
// for - tenants, tenant_members, invitations - as one consolidated type
// (rather than three separate repo structs), matching the package-per-
// bounded-context pattern's "one repository.go per context" shape. Method
// names are prefixed by entity (CreateTenant/CreateMember/CreateInvitation,
// etc.) where the bare CRUD verb would otherwise collide across the three.
type Repository struct {
	db      *database.DB
	adminDB *database.DB
}

func NewRepository(db, adminDB *database.DB) *Repository {
	return &Repository{db: db, adminDB: adminDB}
}

// ==================== tenants ====================
//
// Unlike tenant_members/invitations, tenants has no tenant_id column (it
// IS the tenant) and therefore no RLS policy - membership rows are what's
// protected, and a caller can only ever reach a tenant's id in the first
// place by already being one of its members (enforced by the service
// layer / middleware.RequireTenantForUser) or being the platform.

const tenantColumns = `id, name, slug, owner_user_id, status, status_reason, created_at, updated_at`

func (r *Repository) CreateTenant(ctx context.Context, t *Tenant) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO tenants (id, name, slug, owner_user_id, status, status_reason, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, t.Name, t.Slug, t.OwnerUserID, t.Status, t.StatusReason, t.CreatedAt, t.UpdatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

func (r *Repository) GetTenantByID(ctx context.Context, id string) (*Tenant, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE id = $1`, id)
	return scanTenant(row)
}

func (r *Repository) GetTenantBySlug(ctx context.Context, slug string) (*Tenant, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE slug = $1`, slug)
	return scanTenant(row)
}

// UpdateTenantStatus transitions a tenant to a new status, setting (or
// clearing, when reason is nil - e.g. reactivating a Suspended tenant back
// to Active) status_reason in the same statement. Called by
// Service.SetTenantStatus, the only path that ever writes these two
// columns together.
func (r *Repository) UpdateTenantStatus(ctx context.Context, tenantID string, status TenantStatus, reason *string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE tenants SET status = $2, status_reason = $3 WHERE id = $1`,
		tenantID, status, reason,
	)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// TenantWithRole pairs a Tenant with the caller's role in it - the shape
// ListTenantsForUser needs for a tenant-switcher UI.
type TenantWithRole struct {
	Tenant
	Role MemberRole
}

// ListTenantsForUser returns every tenant a user owns or is staff of,
// joining through tenant_members. It deliberately runs on the BYPASSRLS
// admin connection (see DBConfig.AdminDSN): tenant_members' RLS policy
// only grants visibility inside a specific tenant's transaction, but
// "which tenants am I in" is, by definition, a cross-tenant question - and
// it is safe to answer here without a tenant context because the WHERE
// clause is pinned to userID, which the caller (middleware.
// RequireUserAuth / GET /api/v1/tenants/me) has already authenticated via
// JWT. A caller can therefore only ever list their OWN memberships, never
// anyone else's.
func (r *Repository) ListTenantsForUser(ctx context.Context, userID string) ([]TenantWithRole, error) {
	rows, err := r.adminDB.DB.QueryContext(ctx, `
		SELECT t.id, t.name, t.slug, t.owner_user_id, t.status, t.status_reason, t.created_at, t.updated_at, tm.role
		FROM tenants t
		JOIN tenant_members tm ON tm.tenant_id = t.id
		WHERE tm.user_id = $1 AND tm.status = 'active'
		ORDER BY t.created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TenantWithRole
	for rows.Next() {
		var t Tenant
		var role MemberRole
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.OwnerUserID, &t.Status, &t.StatusReason, &t.CreatedAt, &t.UpdatedAt, &role); err != nil {
			return nil, err
		}
		out = append(out, TenantWithRole{Tenant: t, Role: role})
	}
	return out, rows.Err()
}

func scanTenant(row dbutil.RowScanner) (*Tenant, error) {
	t := &Tenant{}
	err := row.Scan(&t.ID, &t.Name, &t.Slug, &t.OwnerUserID, &t.Status, &t.StatusReason, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return t, nil
}

// ==================== tenant_members ====================
//
// Every method below is strictly tenant-scoped: it must be called with a
// context produced by database.DB.WithTenantTx, or the RLS policy on
// tenant_members (migrations/0003_row_level_security.up.sql) will make it
// see zero rows. This is deliberate and has a consequence for callers:
// internal/httpapi/middleware/tenant.go resolves the tenant requested by
// the caller (header/subdomain) and opens a WithTenantTx transaction that
// wraps the *entire* rest of the request - including the ActiveRole call
// below that checks "is this user actually a member of the tenant they
// are asking to act as" - rather than checking membership before opening
// tenant context. There is no bypass path here; unlike
// GetInvitationByTokenHash (below) or internal/oauthclient's
// Repository.GetByClientID (which authenticate a caller who presents an
// unguessable secret and therefore need a pre-context lookup), membership
// checks authorize a caller who has already authenticated as a specific
// user and is now declaring which tenant they want to act as, so requiring
// that declaration to open real tenant context first is both simpler and
// strictly safer.

const membershipColumns = `id, tenant_id, user_id, role, status, created_at, updated_at`

func (r *Repository) CreateMember(ctx context.Context, m *TenantMember) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO tenant_members (id, tenant_id, user_id, role, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		m.ID, m.TenantID, m.UserID, m.Role, m.Status, m.CreatedAt, m.UpdatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

// GetMember looks up the caller's own membership inside the active tenant
// context.
func (r *Repository) GetMember(ctx context.Context, tenantID, userID string) (*TenantMember, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `
		SELECT `+membershipColumns+` FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID)
	return scanMembership(row)
}

// ActiveRole implements internal/httpapi/middleware's MembershipChecker
// interface structurally (middleware never imports this package - see
// docs/phase0-refactor-plan.md §4.2). It is a thin wrapper around
// GetMember + the MemberStatusActive check that already lived in
// middleware.RequireTenantForUser before this move: err is reserved for
// genuine infra failures, and "no such membership" collapses into
// active=false rather than the caller having to compare against
// domain.ErrNotFound itself.
func (r *Repository) ActiveRole(ctx context.Context, tenantID, userID string) (role string, active bool, err error) {
	m, err := r.GetMember(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(m.Role), m.Status == MemberStatusActive, nil
}

// ListMembers returns tenant_members newest-first, keyset-paginated by
// (created_at, id) - see internal/platform/pagination's doc comment for why.
func (r *Repository) ListMembers(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[TenantMember], error) {
	limit := page.NormalizeLimit()
	query := `SELECT ` + membershipColumns + ` FROM tenant_members WHERE tenant_id = $1`
	args := []any{tenantID}

	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		query += ` AND (created_at, id) < ($2, $3)`
		args = append(args, c.CreatedAt, c.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprint(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[TenantMember]{}, err
	}
	defer rows.Close()

	var out []TenantMember
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return pagination.Page[TenantMember]{}, err
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[TenantMember]{}, err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return pagination.Page[TenantMember]{Items: out, NextCursor: next}, nil
}

func scanMembership(row dbutil.RowScanner) (*TenantMember, error) {
	m := &TenantMember{}
	err := row.Scan(&m.ID, &m.TenantID, &m.UserID, &m.Role, &m.Status, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return m, nil
}

// ==================== invitations ====================
//
// Tenant-scoped (see the tenant_members section above for the general
// pattern), except GetInvitationByTokenHash which an invitee - who is not
// yet a tenant member and therefore cannot open a tenant transaction -
// uses to redeem their invitation token.

const invitationColumns = `id, tenant_id, email, role, token_hash, invited_by, status, expires_at, accepted_at, created_at`

func (r *Repository) CreateInvitation(ctx context.Context, inv *Invitation) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO invitations (id, tenant_id, email, role, token_hash, invited_by, status, expires_at, accepted_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		inv.ID, inv.TenantID, inv.Email, inv.Role, inv.TokenHash, inv.InvitedBy,
		inv.Status, inv.ExpiresAt, inv.AcceptedAt, inv.CreatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

// ListInvitations returns invitations newest-first, keyset-paginated by
// (created_at, id) - see internal/platform/pagination's doc comment for why.
func (r *Repository) ListInvitations(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[Invitation], error) {
	limit := page.NormalizeLimit()
	query := `SELECT ` + invitationColumns + ` FROM invitations WHERE tenant_id = $1`
	args := []any{tenantID}

	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		query += ` AND (created_at, id) < ($2, $3)`
		args = append(args, c.CreatedAt, c.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprint(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[Invitation]{}, err
	}
	defer rows.Close()

	var out []Invitation
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return pagination.Page[Invitation]{}, err
		}
		out = append(out, *inv)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[Invitation]{}, err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return pagination.Page[Invitation]{Items: out, NextCursor: next}, nil
}

// GetInvitationByTokenHash looks up an invitation by the SHA-256 hash of
// the raw token embedded in the invite link, using the BYPASSRLS admin
// connection. The invitee is, by construction, not yet a member of the
// tenant, so no tenant context can exist yet to satisfy the normal RLS
// policy; this is safe precisely because the lookup key (a 24-byte random
// token's hash) is an unguessable secret the invitee must already possess
// - the same "possession of the secret is the authorization" pattern used
// for the (unscoped, RLS-free) email_verification_tokens table.
func (r *Repository) GetInvitationByTokenHash(ctx context.Context, tokenHash string) (*Invitation, error) {
	row := r.adminDB.DB.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE token_hash = $1`, tokenHash)
	return scanInvitation(row)
}

// MarkInvitationAccepted transitions an invitation to accepted. Called
// from within the same tenant transaction used to create the resulting
// tenant_member row (see Service.AcceptInvitation), so it is tenant-scoped
// even though the initial lookup was not.
func (r *Repository) MarkInvitationAccepted(ctx context.Context, id string, acceptedAt time.Time) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE invitations SET status = 'accepted', accepted_at = $2 WHERE id = $1 AND status = 'pending'`,
		id, acceptedAt)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func (r *Repository) RevokeInvitation(ctx context.Context, tenantID, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE invitations SET status = 'revoked' WHERE id = $1 AND tenant_id = $2 AND status = 'pending'`,
		id, tenantID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func scanInvitation(row dbutil.RowScanner) (*Invitation, error) {
	inv := &Invitation{}
	err := row.Scan(
		&inv.ID, &inv.TenantID, &inv.Email, &inv.Role, &inv.TokenHash, &inv.InvitedBy,
		&inv.Status, &inv.ExpiresAt, &inv.AcceptedAt, &inv.CreatedAt,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return inv, nil
}
