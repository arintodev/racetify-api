package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
	"github.com/racetify/racetify-api/internal/security"
)

// Google sign-in with a BFF in front of the API, including tenants on
// custom domains, cannot hand a session back from the OAuth callback: the
// callback lands on one fixed host (Google's redirect_uri), while the user
// started on another. So the callback only proves who the user is and
// redirects the browser back to the originating frontend host carrying a
// short-lived, single-use exchange code; that host's BFF then redeems the
// code server-to-server for the actual session.
//
// A Google identity with no account yet does NOT get one created at the
// callback: the sign-up UX requires explicit consent to the Terms and
// Privacy Policy first, so the exchange returns a pending profile and the
// account is created by CompleteGoogleSignup.

const (
	googleExchangeCodeTTL = time.Minute
	googlePendingTTL      = 15 * time.Minute
)

func googleCodeKey(codeHash string) string     { return "auth:google:code:" + codeHash }
func googlePendingKey(tokenHash string) string { return "auth:google:pending:" + tokenHash }

// googleCodePayload is what an exchange code stands for: either an
// existing user to sign in, or an identity that still needs a profile.
type googleCodePayload struct {
	UserID string          `json:"user_id,omitempty"`
	Info   *GoogleUserInfo `json:"info,omitempty"`
}

// ResolveGoogleIdentity turns a verified Google identity into a one-time
// exchange code. An existing account (matched by provider id, or linked by
// verified email) is signed in at exchange time; otherwise the code leads
// to a pending sign-up.
func (s *Service) ResolveGoogleIdentity(ctx context.Context, info *GoogleUserInfo) (exchangeCode string, err error) {
	if !info.EmailVerified {
		// Linking or creating an account from an unverified email would
		// allow taking over someone else's account.
		return "", domain.ErrForbidden
	}
	email := normalizeEmail(info.Email)

	var payload googleCodePayload
	err = s.db.WithTx(ctx, func(ctx context.Context) error {
		byGoogle, err := s.repo.GetUserByProviderID(ctx, SocialProviderGoogle, info.Sub)
		if err == nil {
			payload.UserID = byGoogle.ID
			return nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}

		byEmail, err := s.repo.GetUserByEmail(ctx, email)
		if err == nil {
			// Existing email/password account signing in with Google for
			// the first time: link the identities rather than creating a
			// duplicate, since "Single Runner Identity" is keyed on email.
			if err := s.repo.CreateSocialAccount(ctx, &SocialAccount{
				ID:             security.MustNewUUIDv4(),
				UserID:         byEmail.ID,
				Provider:       SocialProviderGoogle,
				ProviderUserID: info.Sub,
				CreatedAt:      time.Now().UTC(),
			}); err != nil {
				return err
			}
			payload.UserID = byEmail.ID
			return nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}

		payload.Info = info
		return nil
	})
	if err != nil {
		return "", err
	}
	return s.storeGoogleCode(ctx, payload)
}

func (s *Service) storeGoogleCode(ctx context.Context, payload googleCodePayload) (string, error) {
	raw, err := security.GenerateOpaqueToken(32)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if err := s.redis.Set(ctx, googleCodeKey(security.HashToken(raw)), string(b), googleExchangeCodeTTL); err != nil {
		return "", err
	}
	return raw, nil
}

// GoogleExchangeResult is either a signed-in session (User + Tokens) or a
// pending sign-up (PendingToken + Profile), never both.
type GoogleExchangeResult struct {
	User         *User
	Tokens       *SessionTokens
	PendingToken string
	Profile      *GoogleProfile
}

// GoogleProfile is the pre-filled data shown in the consent step.
type GoogleProfile struct {
	Email     string
	FirstName string
	LastName  string
}

// ExchangeGoogleCode redeems a single-use exchange code.
func (s *Service) ExchangeGoogleCode(ctx context.Context, code string, client ClientInfo) (*GoogleExchangeResult, error) {
	key := googleCodeKey(security.HashToken(code))
	raw, err := s.redis.Get(ctx, key)
	if err != nil {
		if errors.Is(err, rediscli.ErrNil) {
			return nil, domain.ErrTokenExpired
		}
		return nil, err
	}
	_ = s.redis.Del(ctx, key) // single use

	var payload googleCodePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, err
	}

	if payload.UserID != "" {
		user, err := s.repo.GetUserByID(ctx, payload.UserID)
		if err != nil {
			return nil, err
		}
		if user.Status != UserStatusActive {
			return nil, domain.ErrAccountSuspended
		}
		_ = s.db.WithTx(ctx, func(ctx context.Context) error {
			return s.recordAudit(ctx, nil, &user.ID, nil, audit.ActionUserLoggedIn,
				map[string]any{"via": "google", "ip": client.IP})
		})
		tokens, err := s.issueSession(ctx, user, client)
		if err != nil {
			return nil, err
		}
		return &GoogleExchangeResult{User: user, Tokens: tokens}, nil
	}

	if payload.Info == nil {
		return nil, domain.ErrTokenExpired
	}
	pending, err := security.GenerateOpaqueToken(32)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(payload.Info)
	if err != nil {
		return nil, err
	}
	if err := s.redis.Set(ctx, googlePendingKey(security.HashToken(pending)), string(b), googlePendingTTL); err != nil {
		return nil, err
	}
	first, last := payload.Info.GivenName, payload.Info.FamilyName
	if first == "" && last == "" {
		first, last = splitFullName(payload.Info.Name)
	}
	return &GoogleExchangeResult{
		PendingToken: pending,
		Profile:      &GoogleProfile{Email: normalizeEmail(payload.Info.Email), FirstName: first, LastName: last},
	}, nil
}

// CompleteGoogleSignup creates the account for a pending Google identity
// once the user has confirmed their name and accepted the terms.
func (s *Service) CompleteGoogleSignup(ctx context.Context, pendingToken, firstName, lastName string, acceptedTerms bool, client ClientInfo) (*User, *SessionTokens, error) {
	if !acceptedTerms {
		return nil, nil, domain.ErrTermsNotAccepted
	}
	key := googlePendingKey(security.HashToken(pendingToken))
	raw, err := s.redis.Get(ctx, key)
	if err != nil {
		if errors.Is(err, rediscli.ErrNil) {
			return nil, nil, domain.ErrTokenExpired
		}
		return nil, nil, err
	}
	var info GoogleUserInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	user := &User{
		ID:              security.MustNewUUIDv4(),
		Email:           normalizeEmail(info.Email),
		FirstName:       strings.TrimSpace(firstName),
		LastName:        strings.TrimSpace(lastName),
		IsEmailVerified: true,
		Status:          UserStatusActive,
		TermsAcceptedAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	err = s.db.WithTx(ctx, func(ctx context.Context) error {
		if err := s.repo.CreateUser(ctx, user); err != nil {
			return err
		}
		if err := s.repo.CreateSocialAccount(ctx, &SocialAccount{
			ID:             security.MustNewUUIDv4(),
			UserID:         user.ID,
			Provider:       SocialProviderGoogle,
			ProviderUserID: info.Sub,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
		return s.recordAudit(ctx, nil, &user.ID, nil, audit.ActionUserRegistered,
			map[string]any{"via": "google", "ip": client.IP})
	})
	if err != nil {
		return nil, nil, err
	}
	_ = s.redis.Del(ctx, key) // single use

	tokens, err := s.issueSession(ctx, user, client)
	if err != nil {
		return nil, nil, err
	}
	return user, tokens, nil
}
