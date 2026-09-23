package oauthclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/platform/ratelimit"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
)

// Service implements "Server-to-Server Credentials Provisioning"
// (management, by an authenticated Tenant Owner/Admin) and the "OAuth 2.0
// Client Credentials Grant Flow" itself (POST /oauth/token, called by the
// unauthenticated external backend presenting client_id + client_secret).
type Service struct {
	db      *database.DB
	clients *Repository
	audit   *audit.Repository
	tokens  *security.TokenManager
	limiter *ratelimit.Limiter
	cfg     config.AuthConfig
}

func NewService(
	db *database.DB,
	clients *Repository,
	audit *audit.Repository,
	tokens *security.TokenManager,
	limiter *ratelimit.Limiter,
	cfg config.AuthConfig,
) *Service {
	return &Service{db: db, clients: clients, audit: audit, tokens: tokens, limiter: limiter, cfg: cfg}
}

// CreateClient provisions a new M2M credential. Returns the plaintext
// secret exactly once - the caller (HTTP handler) must hand it back in the
// response body and nowhere else; only its hash is ever persisted.
func (s *Service) CreateClient(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, name string, scopes []string) (*OAuthClient, string, error) {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return nil, "", domain.ErrForbidden
	}
	for _, sc := range scopes {
		if !IsValidScope(sc) {
			return nil, "", fmt.Errorf("service: %w: %q", domain.ErrInvalidScope, sc)
		}
	}
	if strings.TrimSpace(name) == "" {
		return nil, "", fmt.Errorf("service: %w: client name is required", domain.ErrInvalidState)
	}

	clientID, err := security.GenerateOAuthClientID()
	if err != nil {
		return nil, "", err
	}
	clientSecret, err := security.GenerateOAuthClientSecret()
	if err != nil {
		return nil, "", err
	}

	now := time.Now().UTC()
	client := &OAuthClient{
		ID:               security.MustNewUUIDv4(),
		TenantID:         tenantID,
		ClientID:         clientID,
		ClientSecretHash: security.HashToken(clientSecret),
		Name:             name,
		Scopes:           scopes,
		Status:           OAuthClientStatusActive,
		CreatedBy:        actorUserID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.clients.Create(ctx, client); err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionOAuthClientCreated,
			map[string]any{"client_id": clientID, "scopes": scopes})
	})
	if err != nil {
		return nil, "", err
	}
	return client, clientSecret, nil
}

// ListClients returns one keyset-paginated page of tenantID's OAuth
// clients - see internal/platform/pagination for why keyset rather
// than LIMIT/OFFSET.
func (s *Service) ListClients(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[OAuthClient], error) {
	var out pagination.Page[OAuthClient]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.clients.List(ctx, tenantID, page)
		return err
	})
	return out, err
}

func (s *Service) RevokeClient(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, clientRowID string) error {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return domain.ErrForbidden
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.clients.Revoke(ctx, tenantID, clientRowID); err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionOAuthClientRevoked, map[string]any{"id": clientRowID})
	})
}

// GrantResult mirrors RFC 6749 §5.1's client_credentials success response
// shape.
type GrantResult struct {
	AccessToken string
	TokenType   string
	ExpiresIn   int64 // seconds
	Scope       string
}

// ClientCredentialsGrant implements POST /oauth/token: verifies
// client_id/client_secret, narrows to the requested (or, if omitted, the
// full granted) scope set, and issues a scoped JWT carrying the client's
// fixed tenant_id - the exact flow diagrammed in the implementation guide.
func (s *Service) ClientCredentialsGrant(ctx context.Context, clientID, clientSecret, requestedScope string) (*GrantResult, error) {
	client, err := s.clients.GetByClientID(ctx, clientID)
	if err != nil {
		if err == domain.ErrNotFound {
			return nil, domain.ErrInvalidCredentials
		}
		return nil, err
	}
	if client.Status != OAuthClientStatusActive {
		return nil, domain.ErrInvalidCredentials
	}
	if !security.SecureCompare(security.HashToken(clientSecret), client.ClientSecretHash) {
		return nil, domain.ErrInvalidCredentials
	}

	grantedScopes := client.Scopes
	if requestedScope != "" {
		requested := strings.Fields(requestedScope)
		for _, rs := range requested {
			if !client.HasScope(rs) {
				return nil, fmt.Errorf("service: %w: %q was not granted to this client", domain.ErrInvalidScope, rs)
			}
		}
		grantedScopes = requested
	}

	access, _, expiresAt, err := s.tokens.IssueM2MAccessToken(client.ClientID, client.TenantID, grantedScopes, s.cfg.M2MTokenTTL)
	if err != nil {
		return nil, err
	}

	s.clients.TouchLastUsed(ctx, client.ID, time.Now().UTC())

	_ = s.db.WithTenantTx(ctx, client.TenantID, func(ctx context.Context) error {
		return s.recordAudit(ctx, &client.TenantID, nil, &client.ClientID, audit.ActionOAuthTokenIssued,
			map[string]any{"scope": grantedScopes})
	})

	return &GrantResult{
		AccessToken: access,
		TokenType:   "Bearer",
		ExpiresIn:   int64(time.Until(expiresAt).Seconds()),
		Scope:       strings.Join(grantedScopes, " "),
	}, nil
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
