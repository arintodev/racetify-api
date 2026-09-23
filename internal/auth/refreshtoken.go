package auth

import "time"

// RefreshToken backs the httponly-cookie session refresh flow: the raw
// token is only ever sent to the client once (at login/refresh time); the
// database stores nothing but its SHA-256 hash, exactly like an
// OAuthClient secret.
type RefreshToken struct {
	ID             string
	UserID         string
	TokenHash      string
	ReplacedByHash *string // set on rotation, enables reuse detection
	UserAgent      *string
	IPAddress      *string
	ExpiresAt      time.Time
	RevokedAt      *time.Time
	CreatedAt      time.Time
}

func (t *RefreshToken) IsActive(now time.Time) bool {
	return t.RevokedAt == nil && now.Before(t.ExpiresAt)
}

// EmailTokenPurpose distinguishes the different single-use tokens issued
// to a user's inbox.
type EmailTokenPurpose string

const (
	PurposeEmailVerify   EmailTokenPurpose = "email_verify"
	PurposePasswordReset EmailTokenPurpose = "password_reset"
)

type EmailVerificationToken struct {
	ID         string
	UserID     string
	TokenHash  string
	Purpose    EmailTokenPurpose
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

func (t *EmailVerificationToken) IsUsable(now time.Time) bool {
	return t.ConsumedAt == nil && now.Before(t.ExpiresAt)
}
