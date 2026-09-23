package auth

import "time"

// SocialProvider identifies which external identity provider a
// SocialAccount links to. Kept as a plain string, not a DB-level enum -
// see migrations/0001_core.up.sql's doc comment: adding a second provider
// (Facebook, Apple, ...) must never require an ALTER TABLE/ALTER TYPE
// migration, just a new constant here.
type SocialProvider string

const (
	SocialProviderGoogle SocialProvider = "google"
)

// SocialAccount links a User to an identity at an external provider. This
// lives in its own table (user_social_accounts) rather than as a
// google_id column on users, for two reasons: a second provider (Apple,
// Facebook, ...) needs a new row, not a new users column, and a user
// linking multiple providers is naturally multiple rows instead of an
// ever-widening users table. See Repository's social-accounts section for
// how a sign-in looks this up.
type SocialAccount struct {
	ID             string
	UserID         string
	Provider       SocialProvider
	ProviderUserID string // the provider's own subject/user id (Google's "sub")
	CreatedAt      time.Time
}
