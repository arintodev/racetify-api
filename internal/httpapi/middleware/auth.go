package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/security"
)

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix)), true
}

// RequireUserAuth verifies a `typ=user_access` JWT and rejects tokens that
// were blacklisted by an explicit logout (see AuthService.Logout, passed
// in here as isRevoked to avoid this package importing service - service
// stays a strict "no net/http" layer). On success it stashes the user id,
// super-admin flag, access-token metadata (jti/exp - needed by the logout
// handler itself), and - if the token carries one (see Claims.TenantID's
// doc comment in internal/security/jwt.go) - the tenant it was scoped to
// at issuance, into context. RequireTenantForUser (chained after this
// when a route needs one) reads that tenant id back out of context and
// re-verifies it against the database; it never trusts this claim on its
// own.
func RequireUserAuth(tokens *security.TokenManager, isRevoked func(ctx context.Context, jti string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				respond.Error(w, http.StatusUnauthorized, "unauthorized", "Missing or malformed Authorization header.")
				return
			}
			claims, err := tokens.Parse(raw)
			if err != nil || claims.TokenType != security.TokenTypeUserAccess {
				respond.Error(w, http.StatusUnauthorized, "unauthorized", "Invalid or expired access token.")
				return
			}
			if isRevoked(r.Context(), claims.ID) {
				respond.Error(w, http.StatusUnauthorized, "unauthorized", "This session has been signed out.")
				return
			}

			ctx := reqctx.WithUserID(r.Context(), claims.UserID)
			ctx = reqctx.WithSuperAdmin(ctx, claims.IsSuperAdmin)
			if claims.TenantID != "" {
				ctx = reqctx.WithTenantID(ctx, claims.TenantID)
			}
			var expUnix int64
			if claims.ExpiresAt != nil {
				expUnix = claims.ExpiresAt.Unix()
			}
			ctx = reqctx.WithAccessTokenMeta(ctx, claims.ID, expUnix)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireM2MAuth verifies a `typ=m2m_access` JWT (issued by
// OAuthClientService.ClientCredentialsGrant) and stashes the client id,
// its fixed tenant_id, and its granted scopes into context.
func RequireM2MAuth(tokens *security.TokenManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				respond.Error(w, http.StatusUnauthorized, "unauthorized", "Missing or malformed Authorization header.")
				return
			}
			claims, err := tokens.Parse(raw)
			if err != nil || claims.TokenType != security.TokenTypeM2MAccess {
				respond.Error(w, http.StatusUnauthorized, "unauthorized", "Invalid or expired access token.")
				return
			}

			ctx := reqctx.WithM2MClientID(r.Context(), claims.ClientID)
			ctx = reqctx.WithTenantID(ctx, claims.TenantID)
			ctx = reqctx.WithM2MScopes(ctx, claims.Scopes)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireScope authorizes an M2M-authenticated request against the scopes
// baked into its access token at grant time.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, s := range reqctx.M2MScopes(r.Context()) {
				if s == scope {
					next.ServeHTTP(w, r)
					return
				}
			}
			respond.Error(w, http.StatusForbidden, "insufficient_scope", "This client was not granted the '"+scope+"' scope.")
		})
	}
}
