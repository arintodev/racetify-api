package security

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token types embedded in the `typ` claim, used both for readability and
// so a token minted for one purpose can never be replayed as another (e.g.
// an M2M token can never be accepted where a user access token is
// expected, even though both are HS256 JWTs signed with the same secret).
const (
	TokenTypeUserAccess = "user_access"
	TokenTypeM2MAccess  = "m2m_access"
)

// Claims is the single JWT claim set used for both user-session access
// tokens and Server-to-Server (client_credentials) access tokens. Keeping
// one struct (rather than two) means one Parse path and one place that
// enforces "iss/exp/nbf are always checked".
type Claims struct {
	jwt.RegisteredClaims

	TokenType string `json:"typ"`

	// User access token fields.
	UserID       string `json:"uid,omitempty"`
	IsSuperAdmin bool   `json:"super,omitempty"`

	// M2M (client_credentials) access token fields.
	ClientID string   `json:"cid,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`

	// TenantID is the tenant this token is scoped to. For an M2M token
	// it's the tenant the OAuth client was provisioned under - fixed at
	// credential creation time, which is what lets RequireTenantForM2M
	// trust it directly instead of doing a DB lookup per request. For a
	// user access token it starts empty (right after Login/Google
	// sign-in, until a tenant is selected) and is only ever set by
	// auth.Service, which re-verifies membership (Service.
	// verifyTenantMembership) before minting a token that carries one -
	// see Login's optional tenant_id and POST /auth/switch-tenant
	// (Service.SelectTenant). Embedding it here never substitutes for
	// RequireTenantForUser's own per-request DB membership check; it only
	// relocates *which* tenant the caller is asking to act as from a
	// per-request header to a value already validated at issuance time.
	TenantID string `json:"tid,omitempty"`
}

// HasScope reports whether the M2M token was granted scope.
func (c *Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// TokenManager issues and verifies JWTs. HS256 with a shared secret is the
// Phase 0 baseline the implementation guide calls for; moving to an
// asymmetric algorithm (RS256/ES256) later only touches this type.
type TokenManager struct {
	secret []byte
	issuer string
}

func NewTokenManager(secret, issuer string) *TokenManager {
	return &TokenManager{secret: []byte(secret), issuer: issuer}
}

// IssueUserAccessToken mints a short-lived access token for an
// authenticated Runner / Tenant Owner / Tenant Staff session, optionally
// scoped to a tenant (tenantID == "" mints a tenant-less token, valid only
// for non-tenant-scoped endpoints such as GET /users/me, GET /tenants/me,
// or POST /auth/switch-tenant itself). Membership is never trusted from a
// stale claim: the caller (auth.Service) has already re-checked it fresh
// against the database immediately before calling this, and
// RequireTenantForUser (internal/httpapi/middleware/tenant.go) re-checks
// it again on every tenant-scoped request afterwards - a membership
// revoked mid-session is still caught before the token's TTL naturally
// expires. What moved is only *where the tenant selection itself is
// declared*: a value the auth service validated at issuance time, not a
// per-request X-Tenant-ID header.
func (tm *TokenManager) IssueUserAccessToken(userID string, isSuperAdmin bool, tenantID string, ttl time.Duration) (string, string, time.Time, error) {
	jti, err := GenerateOpaqueToken(16)
	if err != nil {
		return "", "", time.Time{}, err
	}
	now := time.Now()
	expiresAt := now.Add(ttl)

	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tm.issuer,
			Subject:   userID,
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		TokenType:    TokenTypeUserAccess,
		UserID:       userID,
		IsSuperAdmin: isSuperAdmin,
		TenantID:     tenantID,
	}

	signed, err := tm.sign(claims)
	return signed, jti, expiresAt, err
}

// IssueM2MAccessToken mints a scoped access token for the OAuth 2.0
// Client Credentials grant. TenantID and Scopes are burned into the token
// at issuance from the oauth_clients row, matching the guide's "Auth
// Server ... menerbitkan JWT Access Token yang memuat tenant_id dan scopes
// terotentikasi".
func (tm *TokenManager) IssueM2MAccessToken(clientID, tenantID string, scopes []string, ttl time.Duration) (string, string, time.Time, error) {
	jti, err := GenerateOpaqueToken(16)
	if err != nil {
		return "", "", time.Time{}, err
	}
	now := time.Now()
	expiresAt := now.Add(ttl)

	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tm.issuer,
			Subject:   clientID,
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		TokenType: TokenTypeM2MAccess,
		ClientID:  clientID,
		TenantID:  tenantID,
		Scopes:    scopes,
	}

	signed, err := tm.sign(claims)
	return signed, jti, expiresAt, err
}

func (tm *TokenManager) sign(claims *Claims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(tm.secret)
	if err != nil {
		return "", fmt.Errorf("security: sign jwt: %w", err)
	}
	return signed, nil
}

// Parse verifies signature, issuer, and standard time-based claims, and
// returns the decoded Claims on success.
func (tm *TokenManager) Parse(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("security: unexpected signing method %v", t.Header["alg"])
		}
		return tm.secret, nil
	}, jwt.WithIssuer(tm.issuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("security: parse jwt: %w", err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("security: invalid jwt")
	}
	return claims, nil
}

// ParseIgnoringExpiry verifies signature and issuer exactly like Parse,
// but tolerates an already-expired exp claim (it skips jwt/v5's automatic
// registered-claims validation entirely via WithoutClaimsValidation, then
// re-checks only the issuer itself by hand). It exists for exactly one
// caller: auth.Service.RefreshAccessToken, which uses it to recover the
// tenant a user had selected (Claims.TenantID) from their just-expired
// access token, without the frontend having to separately remember and
// resupply a tenant_id on every refresh. This is safe precisely because
// the signature is still fully verified - a tenant id recovered this way
// can never be forged - and because the caller never trusts it outright:
// Service.verifyTenantMembership re-checks actual membership against the
// database before that tenant id is ever baked into a new token, exactly
// as it would for any other source of a tenant_id. No other caller in
// this codebase should use this method; every live-request auth path
// (RequireUserAuth, RequireM2MAuth) must keep rejecting an expired token
// outright via Parse.
func (tm *TokenManager) ParseIgnoringExpiry(tokenString string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("security: unexpected signing method %v", t.Header["alg"])
		}
		return tm.secret, nil
	}, jwt.WithoutClaimsValidation())
	if err != nil {
		return nil, fmt.Errorf("security: parse jwt: %w", err)
	}
	if claims.Issuer != tm.issuer {
		return nil, fmt.Errorf("security: unexpected issuer %q", claims.Issuer)
	}
	return claims, nil
}
