package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
	"github.com/racetify/racetify-api/internal/security"
)

// GoogleService implements Runner Social Login via Google using the
// OAuth 2.0 Authorization Code flow, calling Google's endpoints directly
// with net/http. Named GoogleService rather than GoogleOAuthService for
// brevity - the package name (auth) plus "OAuth" already says enough.
//
// Why not golang.org/x/oauth2 (the standard library for this in the Go
// ecosystem): this codebase was built in a sandbox whose network egress
// allowlist does not include golang.org/x/* (see README.md "Dependency
// footprint"), so that package could not be fetched. The protocol itself
// is a handful of documented REST calls
// (https://developers.google.com/identity/protocols/oauth2/web-server),
// which is what this file does directly. If network policy later allows
// it, swapping to golang.org/x/oauth2 + google.golang.org/api's
// oauth2/v2 userinfo client only touches this one file.
type GoogleService struct {
	cfg        config.GoogleConfig
	httpClient *http.Client
	redis      *rediscli.Client
}

func NewGoogleService(cfg config.GoogleConfig, redis *rediscli.Client) *GoogleService {
	return &GoogleService{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		redis:      redis,
	}
}

func (g *GoogleService) Enabled() bool {
	return g.cfg.ClientID != "" && g.cfg.ClientSecret != "" && g.cfg.RedirectURL != ""
}

// BeginAuthorization returns the URL the browser should be redirected to,
// and stores a one-time CSRF state nonce in Redis (5 minute TTL) that
// CompleteAuthorization must see again before it will exchange a code.
func (g *GoogleService) BeginAuthorization(ctx context.Context) (redirectURL string, err error) {
	state, err := security.GenerateOpaqueToken(24)
	if err != nil {
		return "", err
	}
	if err := g.redis.Set(ctx, "oauth:google:state:"+state, "1", 5*time.Minute); err != nil {
		return "", fmt.Errorf("service: store oauth state: %w", err)
	}

	q := url.Values{
		"client_id":     {g.cfg.ClientID},
		"redirect_uri":  {g.cfg.RedirectURL},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
		"access_type":   {"online"},
		"prompt":        {"select_account"},
	}
	return "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode(), nil
}

// GoogleUserInfo is the subset of Google's userinfo response this service
// relies on. GivenName/FamilyName populate FirstName/LastName directly when
// present; Name is kept as a fallback for the rare profile where Google
// does not return them separately (splitFullName below).
type GoogleUserInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
}

// CompleteAuthorization validates the CSRF state, exchanges the
// authorization code for tokens, and fetches the user's profile.
func (g *GoogleService) CompleteAuthorization(ctx context.Context, code, state string) (*GoogleUserInfo, error) {
	stateKey := "oauth:google:state:" + state
	ok, err := g.redis.Exists(ctx, stateKey)
	if err != nil {
		return nil, fmt.Errorf("service: check oauth state: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("service: %w: unknown or expired oauth state", domain.ErrInvalidCredentials)
	}
	_ = g.redis.Del(ctx, stateKey) // single use

	accessToken, err := g.exchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}
	return g.fetchUserInfo(ctx, accessToken)
}

func (g *GoogleService) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {g.cfg.ClientID},
		"client_secret": {g.cfg.ClientSecret},
		"redirect_uri":  {g.cfg.RedirectURL},
		"grant_type":    {"authorization_code"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("service: google token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("service: google token exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("service: decode google token response: %w", err)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("service: google token response missing access_token")
	}
	return payload.AccessToken, nil
}

func (g *GoogleService) fetchUserInfo(ctx context.Context, accessToken string) (*GoogleUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v3/userinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("service: google userinfo: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("service: google userinfo failed (%d): %s", resp.StatusCode, string(body))
	}

	var info GoogleUserInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("service: decode google userinfo: %w", err)
	}
	if info.Sub == "" || info.Email == "" {
		return nil, fmt.Errorf("service: incomplete google userinfo response")
	}
	return &info, nil
}

// LoginOrRegisterWithGoogle finds-or-creates a User for a verified Google
// identity and issues a session, exactly like Service.Login does for
// email/password. It is a method on Service (not GoogleService) because it
// needs the user/session repository; GoogleService only talks to Google.
func (s *Service) LoginOrRegisterWithGoogle(ctx context.Context, info *GoogleUserInfo, userAgent, ip string) (*User, *SessionTokens, error) {
	email := normalizeEmail(info.Email)

	var user *User
	err := s.db.WithTx(ctx, func(ctx context.Context) error {
		byGoogle, err := s.repo.GetUserByProviderID(ctx, SocialProviderGoogle, info.Sub)
		if err == nil {
			user = byGoogle
			return nil
		}
		if err != domain.ErrNotFound {
			return err
		}

		byEmail, err := s.repo.GetUserByEmail(ctx, email)
		if err == nil {
			// Existing email/password account signing in with Google for
			// the first time: link the identities rather than creating a
			// duplicate account, since Racetify's "Single Runner Identity"
			// principle is keyed on email.
			if linkErr := s.repo.CreateSocialAccount(ctx, &SocialAccount{
				ID:             security.MustNewUUIDv4(),
				UserID:         byEmail.ID,
				Provider:       SocialProviderGoogle,
				ProviderUserID: info.Sub,
				CreatedAt:      time.Now().UTC(),
			}); linkErr != nil {
				return linkErr
			}
			user = byEmail
			return nil
		}
		if err != domain.ErrNotFound {
			return err
		}

		firstName, lastName := info.GivenName, info.FamilyName
		if firstName == "" && lastName == "" {
			firstName, lastName = splitFullName(info.Name)
		}

		now := time.Now().UTC()
		newUser := &User{
			ID:              security.MustNewUUIDv4(),
			Email:           email,
			FirstName:       firstName,
			LastName:        lastName,
			IsEmailVerified: info.EmailVerified,
			Status:          UserStatusActive,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := s.repo.CreateUser(ctx, newUser); err != nil {
			return err
		}
		if err := s.repo.CreateSocialAccount(ctx, &SocialAccount{
			ID:             security.MustNewUUIDv4(),
			UserID:         newUser.ID,
			Provider:       SocialProviderGoogle,
			ProviderUserID: info.Sub,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
		user = newUser
		return s.recordAudit(ctx, nil, &user.ID, nil, audit.ActionUserRegistered, map[string]any{"via": "google"})
	})
	if err != nil {
		return nil, nil, err
	}

	if user.Status != UserStatusActive {
		return nil, nil, domain.ErrAccountSuspended
	}

	// Google sign-in always yields a tenant-less access token for now,
	// same baseline as Login without tenant_id - the caller follows up
	// with POST /auth/switch-tenant. Wiring an optional tenant_id through
	// the OAuth callback's `state` would be a natural follow-up if the
	// frontend ever needs "sign in with Google directly into tenant X".
	tokens, err := s.issueSession(ctx, user, userAgent, ip)
	if err != nil {
		return nil, nil, err
	}
	return user, tokens, nil
}

// splitFullName is the fallback used only when Google's userinfo response
// omits given_name/family_name (GoogleUserInfo.GivenName/FamilyName), which
// is the normal case - this just avoids losing the name entirely on the
// rare profile where they're blank but "name" is set.
func splitFullName(name string) (firstName, lastName string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ""
	}
	parts := strings.SplitN(name, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}
