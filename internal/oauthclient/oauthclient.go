// Package oauthclient is the bounded context for Server-to-Server
// (Machine-to-Machine) API credentials: provisioning/managing
// oauth_clients (by an authenticated Tenant Owner/Admin) and the OAuth 2.0
// Client Credentials grant itself (POST /oauth/token, called by the
// unauthenticated external backend presenting client_id + client_secret).
// It owns the oauth_clients table end to end - entity, repository,
// service, HTTP handler, and routes - matching the package-per-bounded-
// context pattern used by every other Phase 0/1 module; see
// docs/phase0-refactor-plan.md for the reasoning behind this split.
package oauthclient

import "time"

type OAuthClientStatus string

const (
	OAuthClientStatusActive  OAuthClientStatus = "active"
	OAuthClientStatusRevoked OAuthClientStatus = "revoked"
)

// OAuthClient is a Server-to-Server (Machine-to-Machine) credential a
// tenant provisions for a third-party backend (timing system, payment
// provider, the tenant's own custom website/app) to call Racetify's API
// via the OAuth 2.0 Client Credentials grant.
type OAuthClient struct {
	ID               string
	TenantID         string
	ClientID         string
	ClientSecretHash string
	Name             string
	Scopes           []string
	Status           OAuthClientStatus
	CreatedBy        string
	LastUsedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// HasScope reports whether the client was granted scope at provisioning
// time.
func (c *OAuthClient) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}
