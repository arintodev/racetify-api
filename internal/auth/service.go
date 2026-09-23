// Package auth's service.go implements the "Autentikasi, Registrasi &
// Otorisasi" module of the implementation guide for Runner (and, by
// extension, any User) accounts: self-registration, email verification,
// login, refresh-token rotation, and logout/revocation. Google Sign-In
// (GoogleService, and Service.LoginOrRegisterWithGoogle which needs this
// type's repositories) lives in google_oauth.go alongside it.
package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/mailer"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
	"github.com/racetify/racetify-api/internal/security"
)

// membershipChecker is the minimal membership-lookup surface Login,
// RefreshAccessToken, and SelectTenant need to re-verify a caller-supplied
// tenant_id against the database before it's ever baked into an access
// token. Declared locally (rather than importing internal/tenant) for the
// same reason middleware.MembershipChecker is - see that interface's doc
// comment in internal/httpapi/middleware/tenant.go; *tenant.Repository
// satisfies this interface structurally, and internal/app wires it in.
type membershipChecker interface {
	ActiveRole(ctx context.Context, tenantID, userID string) (role string, active bool, err error)
}

// Service is this bounded context's service type - named Service rather
// than AuthService (which would stutter as auth.AuthService), matching the
// precedent already set by oauthclient.Service/storage.Service/tenant.
// Service.
type Service struct {
	db      *database.DB
	repo    *Repository
	audit   *audit.Repository
	tokens  *security.TokenManager
	redis   *rediscli.Client
	mailer  mailer.Mailer
	cfg     config.AuthConfig
	members membershipChecker
}

func NewService(
	db *database.DB,
	repo *Repository,
	audit *audit.Repository,
	tokens *security.TokenManager,
	redis *rediscli.Client,
	m mailer.Mailer,
	cfg config.AuthConfig,
	members membershipChecker,
) *Service {
	return &Service{db: db, repo: repo, audit: audit, tokens: tokens, redis: redis, mailer: m, cfg: cfg, members: members}
}

// verifyTenantMembership re-validates - inside a tenant-scoped RLS
// transaction, exactly like middleware.RequireTenantForUser - that userID
// is presently an active member of tenantID. Login (optional tenant_id),
// RefreshAccessToken (optional tenant_id), and SelectTenant all call this
// immediately before minting a token that carries tenantID, so a
// stolen/guessed/stale tenant_id can never be baked into a token without a
// genuine, current membership row backing it.
func (s *Service) verifyTenantMembership(ctx context.Context, tenantID, userID string) error {
	var active bool
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		_, active, err = s.members.ActiveRole(ctx, tenantID, userID)
		return err
	})
	if err != nil {
		return err
	}
	if !active {
		return domain.ErrForbidden
	}
	return nil
}

// SessionTokens is what every login-shaped operation (Register does not
// auto-login, Login, RefreshAccessToken, and Google sign-in all) returns
// to the HTTP layer.
type SessionTokens struct {
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string // raw value; caller sets this as an HTTP-only cookie, never returns it in a JSON body
	RefreshTokenExpiresAt time.Time
	// TenantID is "" for a tenant-less access token, otherwise the tenant
	// the access token above is scoped to (see Claims.TenantID's doc
	// comment in internal/security/jwt.go). SelectTenant's result never
	// sets RefreshToken/RefreshTokenExpiresAt - selecting a tenant only
	// ever mints a new access token, the existing refresh token keeps
	// working unchanged.
	TenantID string
}

// Register implements Runner Self-Registration (email + password).
// Verification is required before the account is fully active, per the
// guide, but the account and its login capability are created immediately
// so failed verification-email delivery does not lock the user out of
// requesting a fresh token.
func (s *Service) Register(ctx context.Context, email, password, firstName, lastName string) (*User, error) {
	email = normalizeEmail(email)
	hash, err := security.HashPassword(password)
	if err != nil {
		return nil, err
	}

	id := security.MustNewUUIDv4()
	now := time.Now().UTC()
	user := &User{
		ID:           id,
		Email:        email,
		PasswordHash: &hash,
		FirstName:    firstName,
		LastName:     lastName,
		Status:       UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	var verifyToken string
	err = s.db.WithTx(ctx, func(ctx context.Context) error {
		if err := s.repo.CreateUser(ctx, user); err != nil {
			return err
		}
		var err error
		verifyToken, err = s.issueEmailToken(ctx, user.ID, PurposeEmailVerify, s.cfg.EmailVerifyTokenTTL)
		if err != nil {
			return err
		}
		return s.recordAudit(ctx, nil, &user.ID, nil, audit.ActionUserRegistered, map[string]any{"email": email})
	})
	if err != nil {
		return nil, err
	}

	// Best-effort: a delivery failure should not fail registration itself,
	// the user can request the verification email again.
	_ = s.mailer.SendVerificationEmail(ctx, user.Email, user.DisplayName(), verificationLink(verifyToken))

	return user, nil
}

// VerifyEmail consumes an email-verification token and marks the account
// verified.
func (s *Service) VerifyEmail(ctx context.Context, rawToken string) error {
	return s.db.WithTx(ctx, func(ctx context.Context) error {
		tok, err := s.repo.GetEmailTokenByTokenHash(ctx, security.HashToken(rawToken))
		if err != nil {
			return err
		}
		if tok.Purpose != PurposeEmailVerify {
			return domain.ErrInvalidState
		}
		if tok.ConsumedAt != nil {
			return domain.ErrTokenConsumed
		}
		if time.Now().After(tok.ExpiresAt) {
			return domain.ErrTokenExpired
		}
		if err := s.repo.MarkEmailTokenConsumed(ctx, tok.ID, time.Now().UTC()); err != nil {
			return err
		}
		if err := s.repo.MarkUserEmailVerified(ctx, tok.UserID); err != nil {
			return err
		}
		return s.recordAudit(ctx, nil, &tok.UserID, nil, audit.ActionUserEmailVerified, nil)
	})
}

// Login authenticates by email+password and issues a fresh access +
// refresh token pair. userAgent/ip are stored alongside the refresh token
// purely for the user's own "active sessions" visibility in a later phase.
//
// The access token is always issued tenant-less: at login time the
// caller cannot yet know which tenant_id to ask for (that's precisely
// what GET /api/v1/tenants/me, called right after, is for) - unlike
// RefreshAccessToken, which can recover a *previous* selection from the
// token being refreshed, Login has no such prior token to draw on. The
// expected flow is Login -> GET /api/v1/tenants/me -> POST
// /auth/switch-tenant (Service.SelectTenant).
func (s *Service) Login(ctx context.Context, email, password, userAgent, ip string) (*User, *SessionTokens, error) {
	email = normalizeEmail(email)

	var user *User
	err := s.db.WithTx(ctx, func(ctx context.Context) error {
		var err error
		user, err = s.repo.GetUserByEmail(ctx, email)
		if err != nil {
			if err == domain.ErrNotFound {
				return domain.ErrInvalidCredentials
			}
			return err
		}
		if user.PasswordHash == nil {
			// Google-only account trying password login.
			return domain.ErrInvalidCredentials
		}
		ok, err := security.VerifyPassword(password, *user.PasswordHash)
		if err != nil || !ok {
			return domain.ErrInvalidCredentials
		}
		if user.Status != UserStatusActive {
			return domain.ErrAccountSuspended
		}
		return s.recordAudit(ctx, nil, &user.ID, nil, audit.ActionUserLoggedIn, map[string]any{"ip": ip})
	})
	if err != nil {
		return nil, nil, err
	}

	tokens, err := s.issueSession(ctx, user, userAgent, ip)
	if err != nil {
		return nil, nil, err
	}
	return user, tokens, nil
}

// RefreshAccessToken rotates a refresh token: the presented raw token is
// revoked and replaced by a brand new one (rotation-on-use), and a new
// access token is issued. If a token that was already revoked is
// presented again, every active session for that user is revoked as a
// theft-response, matching standard refresh-token-reuse-detection
// practice.
//
// previousAccessToken is optional and carries no authority of its own -
// it is the caller's just-expired (or about-to-expire) access token, sent
// purely as a hint so the tenant it was scoped to (Claims.TenantID) can be
// carried forward into the freshly-issued one, without the frontend
// having to separately track and resupply a tenant_id on every refresh.
// Nothing about "which tenant" is ever persisted server-side (there is no
// active_tenant_id on the refresh-token record) - this hint, re-derived
// from a token the client already held, is what stands in for that. It's
// read with TokenManager.ParseIgnoringExpiry (signature and issuer are
// still verified; only the expiry check is skipped - see that method's
// doc comment) and, same as everywhere else, membership is re-verified
// fresh before it's trusted. Pass "" to skip the hint entirely and get a
// tenant-less access token back.
//
// Unlike an explicit tenant selection (Login, SelectTenant), a failed
// membership check here does not fail the refresh: it only means the
// hint no longer applies (e.g. the user's membership was revoked since
// the old token was issued), so RefreshAccessToken silently falls back to
// a tenant-less token instead of blocking a refresh the caller didn't
// explicitly ask to be tenant-scoped in the first place.
func (s *Service) RefreshAccessToken(ctx context.Context, rawRefreshToken, previousAccessToken, userAgent, ip string) (*User, *SessionTokens, error) {
	var user *User
	var newRawRefresh string
	var newRefreshExpiresAt time.Time

	err := s.db.WithTx(ctx, func(ctx context.Context) error {
		existing, err := s.repo.GetRefreshTokenByTokenHash(ctx, security.HashToken(rawRefreshToken))
		if err != nil {
			if err == domain.ErrNotFound {
				return domain.ErrInvalidCredentials
			}
			return err
		}

		if existing.RevokedAt != nil {
			// Reuse of an already-rotated/revoked token: possible theft.
			_ = s.repo.RevokeAllRefreshTokensForUser(ctx, existing.UserID, time.Now().UTC())
			return domain.ErrInvalidCredentials
		}
		if time.Now().After(existing.ExpiresAt) {
			return domain.ErrTokenExpired
		}

		user, err = s.repo.GetUserByID(ctx, existing.UserID)
		if err != nil {
			return err
		}
		if user.Status != UserStatusActive {
			return domain.ErrAccountSuspended
		}

		newRaw, newHash, newExpiresAt, err := s.newRefreshToken()
		if err != nil {
			return err
		}
		newRecord := &RefreshToken{
			ID:        security.MustNewUUIDv4(),
			UserID:    user.ID,
			TokenHash: newHash,
			UserAgent: ptrOrNil(userAgent),
			IPAddress: ptrOrNil(ip),
			ExpiresAt: newExpiresAt,
			CreatedAt: time.Now().UTC(),
		}
		if err := s.repo.CreateRefreshToken(ctx, newRecord); err != nil {
			return err
		}
		if err := s.repo.RotateRefreshToken(ctx, existing.ID, newHash, time.Now().UTC()); err != nil {
			return err
		}

		newRawRefresh = newRaw
		newRefreshExpiresAt = newExpiresAt
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// Deliberately outside the rotation transaction above (and using its
	// own separate WithTenantTx, like Login/SelectTenant) - minting an
	// access token touches no database state, and a membership-
	// verification failure here must not roll back the refresh-token
	// rotation that already succeeded: the caller keeps a valid (now
	// tenant-less) session either way, per the guide's "Refresh token
	// tetap dapat digunakan setelah tenant switching" principle.
	var tenantID string
	if previousAccessToken != "" {
		if prevClaims, err := s.tokens.ParseIgnoringExpiry(previousAccessToken); err == nil &&
			prevClaims.TokenType == security.TokenTypeUserAccess && prevClaims.TenantID != "" {
			tenantID = prevClaims.TenantID
		}
		// A malformed/foreign/tenant-less previousAccessToken is not an
		// error - it just means there's no hint to carry forward, exactly
		// like passing "" would.
	}
	if tenantID != "" {
		if err := s.verifyTenantMembership(ctx, tenantID, user.ID); err != nil {
			tenantID = ""
		}
	}

	access, _, accessExpiresAt, err := s.tokens.IssueUserAccessToken(user.ID, user.IsSuperAdmin, tenantID, s.cfg.AccessTokenTTL)
	if err != nil {
		return nil, nil, err
	}

	tokens := &SessionTokens{
		AccessToken:           access,
		AccessTokenExpiresAt:  accessExpiresAt,
		RefreshToken:          newRawRefresh,
		RefreshTokenExpiresAt: newRefreshExpiresAt,
		TenantID:              tenantID,
	}
	return user, tokens, nil
}

// SelectTenant mints a new access token scoped to tenantID for an
// already-authenticated user - the deliberately session-less counterpart
// to the "tenant switching" endpoint in the reference docs: no
// active_tenant_id or token_family is persisted anywhere (see
// membershipChecker's and SessionTokens.TenantID's doc comments), so this
// mutates nothing beyond what verifyTenantMembership reads. The caller's
// existing refresh token keeps working unchanged, since it never encoded
// a tenant to begin with - selecting a tenant only ever affects the
// access token.
//
// A previously-issued access token for a *different* tenant (if any)
// remains valid for that other tenant until it naturally expires -
// RequireTenantForUser re-checks membership on every request regardless,
// so this is not a privilege-escalation window, only "two access tokens
// for two tenants can be live for the same user at once", which is the
// explicit point of not centralizing a single "active" tenant in shared
// session state (see the multi-tab/concurrent-tenant discussion this
// design followed from).
func (s *Service) SelectTenant(ctx context.Context, userID string, isSuperAdmin bool, tenantID string) (*SessionTokens, error) {
	if tenantID == "" {
		return nil, domain.ErrInvalidState
	}
	if err := s.verifyTenantMembership(ctx, tenantID, userID); err != nil {
		return nil, err
	}

	access, _, accessExpiresAt, err := s.tokens.IssueUserAccessToken(userID, isSuperAdmin, tenantID, s.cfg.AccessTokenTTL)
	if err != nil {
		return nil, err
	}

	_ = s.recordAudit(ctx, &tenantID, &userID, nil, audit.ActionUserSwitchedTenant, nil)

	return &SessionTokens{
		AccessToken:          access,
		AccessTokenExpiresAt: accessExpiresAt,
		TenantID:             tenantID,
	}, nil
}

// Logout revokes the presented refresh token and blacklists the current
// access token's jti in Redis until its natural expiry, so a stolen access
// token cannot keep being used for the remainder of its (short) TTL after
// the user explicitly signs out.
func (s *Service) Logout(ctx context.Context, rawRefreshToken string, accessJTI string, accessExpiresAt time.Time, userID string) error {
	if rawRefreshToken != "" {
		if existing, err := s.repo.GetRefreshTokenByTokenHash(ctx, security.HashToken(rawRefreshToken)); err == nil {
			_ = s.repo.RevokeRefreshToken(ctx, existing.ID, time.Now().UTC())
		}
	}
	if accessJTI != "" {
		ttl := time.Until(accessExpiresAt)
		if ttl > 0 {
			_ = s.redis.Set(ctx, blacklistKey(accessJTI), "1", ttl)
		}
	}
	return s.recordAudit(ctx, nil, &userID, nil, audit.ActionUserLoggedOut, nil)
}

// IsAccessTokenRevoked is consulted by the auth middleware on every
// request carrying a user access token.
func (s *Service) IsAccessTokenRevoked(ctx context.Context, jti string) bool {
	exists, err := s.redis.Exists(ctx, blacklistKey(jti))
	if err != nil {
		// Fail open on Redis unavailability would be a security regression
		// for a logout-driven blacklist; fail closed is safer here, but
		// only for the (rare, already-logged-out-token) check itself - the
		// caller decides what "true" means for the rest of the request.
		return true
	}
	return exists
}

// GetUserByID is exposed directly (not wrapped in more business logic)
// for the HTTP layer's GET /api/v1/users/me - see Handler.Me.
func (s *Service) GetUserByID(ctx context.Context, id string) (*User, error) {
	return s.repo.GetUserByID(ctx, id)
}

func blacklistKey(jti string) string { return "auth:blacklist:" + jti }

// issueSession mints a fresh tenant-less access+refresh pair for an
// already-authenticated user (used by Login and by the Google sign-in
// flow - see Login's doc comment for why neither can know a tenant_id up
// front).
func (s *Service) issueSession(ctx context.Context, user *User, userAgent, ip string) (*SessionTokens, error) {
	access, _, accessExpiresAt, err := s.tokens.IssueUserAccessToken(user.ID, user.IsSuperAdmin, "", s.cfg.AccessTokenTTL)
	if err != nil {
		return nil, err
	}

	rawRefresh, refreshHash, refreshExpiresAt, err := s.newRefreshToken()
	if err != nil {
		return nil, err
	}

	record := &RefreshToken{
		ID:        security.MustNewUUIDv4(),
		UserID:    user.ID,
		TokenHash: refreshHash,
		UserAgent: ptrOrNil(userAgent),
		IPAddress: ptrOrNil(ip),
		ExpiresAt: refreshExpiresAt,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.repo.CreateRefreshToken(ctx, record); err != nil {
		return nil, err
	}

	return &SessionTokens{
		AccessToken:           access,
		AccessTokenExpiresAt:  accessExpiresAt,
		RefreshToken:          rawRefresh,
		RefreshTokenExpiresAt: refreshExpiresAt,
	}, nil
}

func (s *Service) newRefreshToken() (raw, hash string, expiresAt time.Time, err error) {
	raw, err = security.GenerateOpaqueToken(32)
	if err != nil {
		return "", "", time.Time{}, err
	}
	hash = security.HashToken(raw)
	expiresAt = time.Now().UTC().Add(s.cfg.RefreshTokenTTL)
	return raw, hash, expiresAt, nil
}

func (s *Service) issueEmailToken(ctx context.Context, userID string, purpose EmailTokenPurpose, ttl time.Duration) (string, error) {
	raw, err := security.GenerateOpaqueToken(24)
	if err != nil {
		return "", err
	}
	tok := &EmailVerificationToken{
		ID:        security.MustNewUUIDv4(),
		UserID:    userID,
		TokenHash: security.HashToken(raw),
		Purpose:   purpose,
		ExpiresAt: time.Now().UTC().Add(ttl),
		CreatedAt: time.Now().UTC(),
	}
	if err := s.repo.CreateEmailToken(ctx, tok); err != nil {
		return "", err
	}
	return raw, nil
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

// normalizeEmail is duplicated in internal/tenant/service.go rather than
// shared - see that copy's doc comment for why (a single, trivial,
// dependency-free string helper is not worth a shared leaf package for two
// callers).
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// verificationLink builds the front-end deep link embedded in transactional
// emails. In Phase 0 this is a placeholder host; wire it to the real
// front-end origin via config once one exists.
func verificationLink(token string) string {
	return fmt.Sprintf("https://app.racetify.id/verify-email?token=%s", token)
}
