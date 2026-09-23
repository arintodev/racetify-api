package oauthclient

import "time"

// These response shapes deliberately never include ClientSecretHash - only
// the plaintext secret returned once at creation (OAuthClientCreatedDTO)
// is ever wired into a JSON response.

type OAuthClientDTO struct {
	ID         string     `json:"id"`
	ClientID   string     `json:"client_id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Status     string     `json:"status"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func oauthClientResponse(c *OAuthClient) OAuthClientDTO {
	return OAuthClientDTO{
		ID: c.ID, ClientID: c.ClientID, Name: c.Name, Scopes: c.Scopes,
		Status: string(c.Status), LastUsedAt: c.LastUsedAt, CreatedAt: c.CreatedAt,
	}
}

// OAuthClientCreatedDTO is only ever returned once, from the creation
// endpoint, and is the sole place the plaintext client secret appears.
type OAuthClientCreatedDTO struct {
	OAuthClientDTO
	ClientSecret string `json:"client_secret"`
}
