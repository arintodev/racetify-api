package oauthclient

import (
	"net/http"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/ratelimit"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

type Handler struct {
	clients *Service
	limiter *ratelimit.Limiter
}

func NewHandler(clients *Service, limiter *ratelimit.Limiter) *Handler {
	return &Handler{clients: clients, limiter: limiter}
}

type createOAuthClientRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// Create handles POST /api/v1/oauth-clients - Server-to-
// Server Credentials Provisioning. Requires RequireTenantForUser +
// RequireRole(RoleAdmin).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())

	var req createOAuthClientRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}

	client, secret, err := h.clients.CreateClient(r.Context(), tenantID, actorUserID, rbac.MemberRole(actorRoleStr), req.Name, req.Scopes)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, OAuthClientCreatedDTO{
		OAuthClientDTO: oauthClientResponse(client),
		ClientSecret:   secret,
	})
}

// List handles GET /api/v1/oauth-clients. Requires
// RequireTenantForUser + RequireRole(RoleAdmin). Paginated: ?limit=&cursor=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	page, err := h.clients.ListClients(r.Context(), tenantID, respond.PageParamsFromRequest(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]OAuthClientDTO, 0, len(page.Items))
	for i := range page.Items {
		out = append(out, oauthClientResponse(&page.Items[i]))
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[OAuthClientDTO]{Items: out, NextCursor: page.NextCursor})
}

// Revoke handles DELETE /api/v1/oauth-clients/{id}.
// Requires RequireTenantForUser + RequireRole(RoleAdmin).
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())
	id := r.PathValue("id")

	if err := h.clients.RevokeClient(r.Context(), tenantID, actorUserID, rbac.MemberRole(actorRoleStr), id); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

// Token handles POST /oauth/token - the OAuth 2.0 Client Credentials grant
// itself (RFC 6749 §4.4), unauthenticated (the request body IS the
// credential). Accepts both the RFC-standard
// application/x-www-form-urlencoded body and JSON, since most S2S HTTP
// clients default to one or the other.
func (h *Handler) Token(w http.ResponseWriter, r *http.Request) {
	grantType, clientID, clientSecret, scope, err := parseTokenRequest(r)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if grantType != "client_credentials" {
		respond.Error(w, http.StatusBadRequest, "unsupported_grant_type", "Only grant_type=client_credentials is supported.")
		return
	}
	if clientID == "" || clientSecret == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "client_id and client_secret are required.")
		return
	}

	// Per-client_id throttle (the guide's "maksimum 1.000 request/menit
	// untuk API umum"); brute-force protection on this endpoint by source
	// IP is applied one layer out, in the router's middleware chain.
	allowed, _ := h.limiter.Allow(r.Context(), "ratelimit:oauth_token_client:"+clientID, 1000, time.Minute)
	if !allowed {
		respond.Error(w, http.StatusTooManyRequests, "rate_limited", "Too many token requests for this client.")
		return
	}

	result, err := h.clients.ClientCredentialsGrant(r.Context(), clientID, clientSecret, scope)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]any{
		"access_token": result.AccessToken,
		"token_type":   result.TokenType,
		"expires_in":   result.ExpiresIn,
		"scope":        result.Scope,
	})
}

func parseTokenRequest(r *http.Request) (grantType, clientID, clientSecret, scope string, err error) {
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var body struct {
			GrantType    string `json:"grant_type"`
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Scope        string `json:"scope"`
		}
		defer r.Body.Close()
		if decErr := respond.JSONDecode(r, &body); decErr != nil {
			return "", "", "", "", decErr
		}
		return body.GrantType, body.ClientID, body.ClientSecret, body.Scope, nil
	}

	if err := r.ParseForm(); err != nil {
		return "", "", "", "", err
	}
	// RFC 6749 also permits HTTP Basic auth for client credentials; support
	// it as a fallback so standard OAuth libraries work unmodified.
	if basicID, basicSecret, ok := r.BasicAuth(); ok && r.PostForm.Get("client_id") == "" {
		return r.PostForm.Get("grant_type"), basicID, basicSecret, r.PostForm.Get("scope"), nil
	}
	return r.PostForm.Get("grant_type"), r.PostForm.Get("client_id"), r.PostForm.Get("client_secret"), r.PostForm.Get("scope"), nil
}
