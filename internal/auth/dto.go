package auth

import "time"

// These response shapes deliberately never include PasswordHash or any
// other secret material - only *_hash fields on the auth structs are
// private data, and none are wired into a JSON response anywhere in this
// package.

type UserDTO struct {
	ID              string    `json:"id"`
	Email           string    `json:"email"`
	FirstName       string    `json:"first_name"`
	LastName        string    `json:"last_name"`
	IsEmailVerified bool      `json:"is_email_verified"`
	IsSuperAdmin    bool      `json:"is_super_admin"`
	CreatedAt       time.Time `json:"created_at"`
}

func userResponse(u *User) UserDTO {
	return UserDTO{
		ID:              u.ID,
		Email:           u.Email,
		FirstName:       u.FirstName,
		LastName:        u.LastName,
		IsEmailVerified: u.IsEmailVerified,
		IsSuperAdmin:    u.IsSuperAdmin,
		CreatedAt:       u.CreatedAt,
	}
}

type SessionDTO struct {
	User                 UserDTO   `json:"user"`
	AccessToken          string    `json:"access_token"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
	TokenType            string    `json:"token_type"`
	// TenantID is omitted (null) for a tenant-less access token - see
	// SessionTokens.TenantID's doc comment. Its presence is how the
	// frontend tells "the token I just got can already call tenant-scoped
	// endpoints" apart from "I still need to call
	// POST /auth/switch-tenant" without decoding the JWT itself.
	TenantID *string `json:"tenant_id,omitempty"`
}

func sessionResponse(u *User, t *SessionTokens) SessionDTO {
	return SessionDTO{
		User:                 userResponse(u),
		AccessToken:          t.AccessToken,
		AccessTokenExpiresAt: t.AccessTokenExpiresAt,
		TokenType:            "Bearer",
		TenantID:             ptrOrNil(t.TenantID),
	}
}

// TenantAccessTokenDTO is SwitchTenant's response shape: only what
// changed (a fresh, tenant-scoped access token) - unlike SessionDTO it
// carries no user or refresh-token material, since SelectTenant touches
// neither.
type TenantAccessTokenDTO struct {
	AccessToken          string    `json:"access_token"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
	TokenType            string    `json:"token_type"`
	TenantID             string    `json:"tenant_id"`
}

func tenantAccessTokenResponse(t *SessionTokens) TenantAccessTokenDTO {
	return TenantAccessTokenDTO{
		AccessToken:          t.AccessToken,
		AccessTokenExpiresAt: t.AccessTokenExpiresAt,
		TokenType:            "Bearer",
		TenantID:             t.TenantID,
	}
}
