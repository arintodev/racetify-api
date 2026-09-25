// Package auth is the bounded context for the "Single Runner Identity":
// platform-level User accounts (this file), refresh/email tokens
// (refreshtoken.go), linked social identities (social_account.go),
// registration/login/session business logic (service.go, google_oauth.go's
// former LoginOrRegisterWithGoogle folded into service.go), and the HTTP
// adapter for all of it (handler.go, routes.go). It is the last bounded
// context extracted per docs/phase0-refactor-plan.md's Step 8 - every other
// context (tenant, oauthclient, storage) already had a small, temporary,
// explicitly-flagged dependency on the old internal/repository.
// UserRepository/internal/domain.User pending this extraction; those are
// updated to depend on auth.Repository/auth.User in the same commit as this
// package's creation. See internal/tenant/service.go's Service type doc
// comment (pre-Step-8) for how that was tracked.
package auth

import "time"

// UserStatus enumerates the lifecycle states of a platform-level account.
// Kept as a plain string, not a DB-level enum/CHECK constraint - see
// migrations/0001_core.up.sql's doc comment for why: a new status must
// never require a migration to become legal to write.
type UserStatus string

const (
	UserStatusActive    UserStatus = "active"
	UserStatusSuspended UserStatus = "suspended"
)

// User is the "Single Runner Identity": a platform-level account, outside
// tenant isolation, that can join any number of tenants as staff/owner and
// independently register for events run by any tenant.
//
// External identities (Google, and any provider added later) are NOT
// columns on this struct - see social_account.go's SocialAccount and its
// doc comment for why they live in their own table instead.
type User struct {
	ID              string
	Email           string
	PasswordHash    *string // nil for an account with no password set (social-login-only)
	FirstName       string
	LastName        string
	Phone           *string
	IsEmailVerified bool
	IsSuperAdmin    bool
	Status          UserStatus
	TermsAcceptedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// DisplayName joins FirstName/LastName for the places that want one string
// to greet or address the user by (transactional emails, invitation
// "invited by X" text) - a computed value, never stored, so the two names
// stay the single source of truth.
func (u User) DisplayName() string {
	if u.LastName == "" {
		return u.FirstName
	}
	return u.FirstName + " " + u.LastName
}
