package auth

import (
	"context"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// Repository owns all four tables this bounded context is responsible for
// - users, refresh_tokens, email_verification_tokens, user_social_accounts
// - as one consolidated type, matching the package-per-bounded-context
// pattern's "one repository.go per context" shape already used by
// internal/tenant. None of the four are tenant-scoped (no RLS, no
// tenant_id column - a platform identity exists outside any tenant), so
// unlike internal/tenant/internal/storage's Repository, there is no
// adminDB: every method runs over the same, single connection. Method
// names are prefixed by entity (CreateUser/CreateRefreshToken/
// CreateEmailToken/CreateSocialAccount, etc.) where the bare CRUD verb
// would otherwise collide across the four.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

// ==================== users ====================

const userColumns = `id, email, password_hash, first_name, last_name, phone, is_email_verified, is_super_admin, status, created_at, updated_at`

func (r *Repository) CreateUser(ctx context.Context, u *User) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, first_name, last_name, phone, is_email_verified, is_super_admin, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		u.ID, u.Email, u.PasswordHash, u.FirstName, u.LastName, u.Phone,
		u.IsEmailVerified, u.IsSuperAdmin, u.Status, u.CreatedAt, u.UpdatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

func (r *Repository) GetUserByID(ctx context.Context, id string) (*User, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	return scanUser(row)
}

func (r *Repository) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email)
	return scanUser(row)
}

func (r *Repository) MarkUserEmailVerified(ctx context.Context, userID string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `UPDATE users SET is_email_verified = true WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func (r *Repository) UpdateUserPasswordHash(ctx context.Context, userID, passwordHash string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, passwordHash)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func scanUser(row dbutil.RowScanner) (*User, error) {
	u := &User{}
	err := row.Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.FirstName, &u.LastName, &u.Phone,
		&u.IsEmailVerified, &u.IsSuperAdmin, &u.Status, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return u, nil
}

// userTableColumns returns userColumns qualified with the "u." alias used
// by GetUserByProviderID's join, so scanUser's column order stays the
// single source of truth instead of being duplicated here.
func userTableColumns() string {
	return "u.id, u.email, u.password_hash, u.first_name, u.last_name, u.phone, u.is_email_verified, u.is_super_admin, u.status, u.created_at, u.updated_at"
}

// ==================== refresh_tokens ====================
//
// Global (not tenant-scoped): a Runner session is not tied to any tenant.

const refreshTokenColumns = `id, user_id, token_hash, replaced_by_hash, user_agent, ip_address, expires_at, revoked_at, created_at`

func (r *Repository) CreateRefreshToken(ctx context.Context, t *RefreshToken) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO refresh_tokens (id, user_id, token_hash, replaced_by_hash, user_agent, ip_address, expires_at, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.ID, t.UserID, t.TokenHash, t.ReplacedByHash, t.UserAgent, t.IPAddress, t.ExpiresAt, t.RevokedAt, t.CreatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

func (r *Repository) GetRefreshTokenByTokenHash(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `SELECT `+refreshTokenColumns+` FROM refresh_tokens WHERE token_hash = $1`, tokenHash)
	return scanRefreshToken(row)
}

// RotateRefreshToken atomically revokes the old token and links it to its
// replacement (for reuse detection: if a revoked token is ever presented
// again, the caller should treat it as a signal of theft and revoke the
// whole session chain).
func (r *Repository) RotateRefreshToken(ctx context.Context, oldID string, newReplacedByHash string, revokedAt time.Time) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE refresh_tokens SET revoked_at = $2, replaced_by_hash = $3 WHERE id = $1`,
		oldID, revokedAt, newReplacedByHash)
	return err
}

func (r *Repository) RevokeAllRefreshTokensForUser(ctx context.Context, userID string, revokedAt time.Time) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE refresh_tokens SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`,
		userID, revokedAt)
	return err
}

func (r *Repository) RevokeRefreshToken(ctx context.Context, id string, revokedAt time.Time) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE id = $1`, id, revokedAt)
	return err
}

func scanRefreshToken(row dbutil.RowScanner) (*RefreshToken, error) {
	t := &RefreshToken{}
	err := row.Scan(&t.ID, &t.UserID, &t.TokenHash, &t.ReplacedByHash, &t.UserAgent, &t.IPAddress, &t.ExpiresAt, &t.RevokedAt, &t.CreatedAt)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return t, nil
}

// ==================== email_verification_tokens ====================

const emailTokenColumns = `id, user_id, token_hash, purpose, expires_at, consumed_at, created_at`

func (r *Repository) CreateEmailToken(ctx context.Context, t *EmailVerificationToken) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO email_verification_tokens (id, user_id, token_hash, purpose, expires_at, consumed_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		t.ID, t.UserID, t.TokenHash, t.Purpose, t.ExpiresAt, t.ConsumedAt, t.CreatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

func (r *Repository) GetEmailTokenByTokenHash(ctx context.Context, tokenHash string) (*EmailVerificationToken, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `SELECT `+emailTokenColumns+` FROM email_verification_tokens WHERE token_hash = $1`, tokenHash)
	return scanEmailToken(row)
}

func (r *Repository) MarkEmailTokenConsumed(ctx context.Context, id string, consumedAt time.Time) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE email_verification_tokens SET consumed_at = $2 WHERE id = $1 AND consumed_at IS NULL`, id, consumedAt)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func scanEmailToken(row dbutil.RowScanner) (*EmailVerificationToken, error) {
	t := &EmailVerificationToken{}
	err := row.Scan(&t.ID, &t.UserID, &t.TokenHash, &t.Purpose, &t.ExpiresAt, &t.ConsumedAt, &t.CreatedAt)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return t, nil
}

// ==================== user_social_accounts ====================

const socialAccountColumns = `id, user_id, provider, provider_user_id, created_at`

// CreateSocialAccount inserts a new provider link. Callers that are
// establishing a brand new user's first social login
// (Service.LoginOrRegisterWithGoogle) must call this inside the same
// transaction as the users insert - see users' table doc comment in
// migrations/0001_core.up.sql for why that atomicity is what now enforces
// the "must have a password or a linked social account" invariant.
func (r *Repository) CreateSocialAccount(ctx context.Context, sa *SocialAccount) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO user_social_accounts (id, user_id, provider, provider_user_id, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		sa.ID, sa.UserID, sa.Provider, sa.ProviderUserID, sa.CreatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

// GetUserByProviderID looks up the User linked to a given (provider,
// provider_user_id) pair via a join against user_social_accounts.
func (r *Repository) GetUserByProviderID(ctx context.Context, provider SocialProvider, providerUserID string) (*User, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `
		SELECT `+userTableColumns()+`
		FROM users u
		JOIN user_social_accounts sa ON sa.user_id = u.id
		WHERE sa.provider = $1 AND sa.provider_user_id = $2`,
		provider, providerUserID,
	)
	return scanUser(row)
}
