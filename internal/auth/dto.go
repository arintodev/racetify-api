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
	User UserDTO `json:"user"`
	// AccessToken/TokenType and RefreshToken are present only for token
	// delivery (see bodyDelivery in handler.go): non-browser clients that
	// hold the tokens themselves. A browser's tokens travel in HttpOnly
	// cookies and never appear in a body.
	AccessToken          string    `json:"access_token,omitempty"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
	TokenType            string    `json:"token_type,omitempty"`
	// TenantID is omitted (null) for a tenant-less access token - see
	// SessionTokens.TenantID's doc comment. Its presence is how the
	// frontend tells "the token I just got can already call tenant-scoped
	// endpoints" apart from "I still need to call
	// POST /auth/switch-tenant" without decoding the JWT itself.
	TenantID *string `json:"tenant_id,omitempty"`

	RefreshToken          string     `json:"refresh_token,omitempty"`
	RefreshTokenExpiresAt *time.Time `json:"refresh_token_expires_at,omitempty"`
}

// sessionResponse shapes a session for the caller. includeTokens is true only
// for token delivery; otherwise the tokens stay in cookies and the body says
// just who is signed in and until when.
func sessionResponse(u *User, t *SessionTokens, includeTokens bool) SessionDTO {
	exp := t.RefreshTokenExpiresAt
	dto := SessionDTO{
		User:                  userResponse(u),
		AccessTokenExpiresAt:  t.AccessTokenExpiresAt,
		TenantID:              ptrOrNil(t.TenantID),
		RefreshTokenExpiresAt: &exp,
	}
	if includeTokens {
		dto.AccessToken = t.AccessToken
		dto.TokenType = "Bearer"
		dto.RefreshToken = t.RefreshToken
	}
	return dto
}

// TenantAccessTokenDTO is SwitchTenant's response shape: only what
// changed (a fresh, tenant-scoped access token) - unlike SessionDTO it
// carries no user or refresh-token material, since SelectTenant touches
// neither. AccessToken/TokenType follow the same delivery rule as SessionDTO.
type TenantAccessTokenDTO struct {
	AccessToken          string    `json:"access_token,omitempty"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
	TokenType            string    `json:"token_type,omitempty"`
	TenantID             string    `json:"tenant_id"`
}

func tenantAccessTokenResponse(t *SessionTokens, includeToken bool) TenantAccessTokenDTO {
	dto := TenantAccessTokenDTO{
		AccessTokenExpiresAt: t.AccessTokenExpiresAt,
		TenantID:             t.TenantID,
	}
	if includeToken {
		dto.AccessToken = t.AccessToken
		dto.TokenType = "Bearer"
	}
	return dto
}
