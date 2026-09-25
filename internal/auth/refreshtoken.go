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
	// FamilyID groups every token descended from one login: rotation keeps
	// it, so a whole session (one host/device) can be revoked as a unit
	// without touching the same user's other sessions.
	FamilyID string
	// ExpiresAt is the idle expiry (re-derived on every rotation);
	// AbsoluteExpiresAt is the hard cap fixed at login that ExpiresAt can
	// never exceed.
	ExpiresAt         time.Time
	AbsoluteExpiresAt time.Time
	OriginHost        *string
	RevokedAt         *time.Time
	CreatedAt         time.Time
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
