package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
//
// returnTo is the (already validated) frontend URL the callback should send
// the browser back to; it is stored with the state so it cannot be tampered
// with in transit.
//
// browserNonce is a secret the caller also hands the browser in a cookie: the
// callback must present it back, which ties the whole attempt to the browser
// that started it. Only its hash is stored.
func (g *GoogleService) BeginAuthorization(ctx context.Context, returnTo, browserNonce string) (redirectURL string, err error) {
	state, err := security.GenerateOpaqueToken(24)
	if err != nil {
		return "", err
	}
	stored, err := json.Marshal(googleState{NonceHash: security.HashToken(browserNonce), ReturnTo: returnTo})
	if err != nil {
		return "", err
	}
	if err := g.redis.Set(ctx, "oauth:google:state:"+state, string(stored), 5*time.Minute); err != nil {
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

// googleState is what the OAuth state key stands for.
type googleState struct {
	NonceHash string `json:"n"`
	ReturnTo  string `json:"r"`
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
func (g *GoogleService) CompleteAuthorization(ctx context.Context, code, state, browserNonce string) (info *GoogleUserInfo, returnTo string, err error) {
	stateKey := "oauth:google:state:" + state
	raw, err := g.redis.Get(ctx, stateKey)
	if err != nil {
		if errors.Is(err, rediscli.ErrNil) {
			return nil, "", fmt.Errorf("service: %w: unknown or expired oauth state", domain.ErrInvalidCredentials)
		}
		return nil, "", fmt.Errorf("service: check oauth state: %w", err)
	}
	_ = g.redis.Del(ctx, stateKey) // single use
	var stored googleState
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, "", fmt.Errorf("service: %w: unreadable oauth state", domain.ErrInvalidCredentials)
	}
	returnTo = stored.ReturnTo
	if browserNonce == "" || subtle.ConstantTimeCompare([]byte(security.HashToken(browserNonce)), []byte(stored.NonceHash)) != 1 {
		return nil, returnTo, fmt.Errorf("service: %w: oauth attempt not started by this browser", domain.ErrInvalidCredentials)
	}

	accessToken, err := g.exchangeCode(ctx, code)
	if err != nil {
		return nil, returnTo, err
	}
	info, err = g.fetchUserInfo(ctx, accessToken)
	return info, returnTo, err
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
