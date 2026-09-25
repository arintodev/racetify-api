package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
	"github.com/racetify/racetify-api/internal/security"
)

// Self-registration is a three-step flow that matches the sign-up UX
// (email -> OTP -> profile + password + consent) and creates the account
// only once ownership of the email is proven:
//
//	StartSignup      sends a 6-digit OTP to the address
//	VerifySignupOTP  exchanges the OTP for a short-lived signup_token
//	CompleteSignup   creates the (already verified) user from the token
//
// All state lives in Redis with a TTL; nothing is written to PostgreSQL
// until CompleteSignup.

const (
	signupOTPMaxAttempts = 5
	signupMaxSendsPerHr  = 5
)

func signupOTPKey(email string) string      { return "auth:signup:otp:" + email }
func signupAttemptsKey(email string) string { return "auth:signup:attempts:" + email }
func signupCooldownKey(email string) string { return "auth:signup:cooldown:" + email }
func signupSendsKey(email string) string    { return "auth:signup:sends:" + email }
func signupTokenKey(tokenHash string) string {
	return "auth:signup:token:" + tokenHash
}

// otpDigest binds the code to the email so a code issued for one address
// can never validate another, and only the digest is stored.
func otpDigest(email, code string) string {
	return security.HashToken(email + ":" + code)
}

func generateOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("auth: generate otp: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// StartSignup issues (or re-issues, as "resend") a verification OTP for
// email. It deliberately behaves identically whether or not the address is
// already registered - no code is sent for a registered address, but the
// caller cannot tell - so the endpoint cannot be used to enumerate
// accounts.
func (s *Service) StartSignup(ctx context.Context, email string) error {
	email = normalizeEmail(email)

	ok, err := s.redis.SetNX(ctx, signupCooldownKey(email), "1", s.cfg.SignupResendCooldown)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrRateLimited
	}
	sends, err := s.redis.Incr(ctx, signupSendsKey(email))
	if err != nil {
		return err
	}
	if sends == 1 {
		_ = s.redis.Expire(ctx, signupSendsKey(email), time.Hour)
	}
	if sends > signupMaxSendsPerHr {
		return domain.ErrRateLimited
	}

	if _, err := s.repo.GetUserByEmail(ctx, email); err == nil {
		return nil // already registered: silently send nothing
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}

	code, err := generateOTP()
	if err != nil {
		return err
	}
	if err := s.redis.Set(ctx, signupOTPKey(email), otpDigest(email, code), s.cfg.SignupOTPTTL); err != nil {
		return err
	}
	_ = s.redis.Del(ctx, signupAttemptsKey(email))

	return s.mailer.SendSignupOTP(ctx, email, code, s.cfg.SignupOTPTTL)
}

// SignupTiming reports the numbers the UI needs to render its countdown.
func (s *Service) SignupTiming() (otpTTL, resendCooldown time.Duration) {
	return s.cfg.SignupOTPTTL, s.cfg.SignupResendCooldown
}

// VerifySignupOTP checks code against the pending OTP for email. After
// signupOTPMaxAttempts wrong guesses the OTP is destroyed and a new one
// must be requested. On success it returns a single-use signup_token.
func (s *Service) VerifySignupOTP(ctx context.Context, email, code string) (string, error) {
	email = normalizeEmail(email)
	code = strings.TrimSpace(code)

	stored, err := s.redis.Get(ctx, signupOTPKey(email))
	if err != nil {
		if errors.Is(err, rediscli.ErrNil) {
			return "", domain.ErrInvalidOTP
		}
		return "", err
	}

	attempts, err := s.redis.Incr(ctx, signupAttemptsKey(email))
	if err != nil {
		return "", err
	}
	if attempts == 1 {
		_ = s.redis.Expire(ctx, signupAttemptsKey(email), s.cfg.SignupOTPTTL)
	}
	if attempts > signupOTPMaxAttempts {
		_ = s.redis.Del(ctx, signupOTPKey(email), signupAttemptsKey(email))
		return "", domain.ErrInvalidOTP
	}

	if !security.SecureCompare(stored, otpDigest(email, code)) {
		return "", domain.ErrInvalidOTP
	}

	_ = s.redis.Del(ctx, signupOTPKey(email), signupAttemptsKey(email))

	token, err := security.GenerateOpaqueToken(32)
	if err != nil {
		return "", err
	}
	if err := s.redis.Set(ctx, signupTokenKey(security.HashToken(token)), email, s.cfg.SignupTokenTTL); err != nil {
		return "", err
	}
	return token, nil
}

// CompleteSignup creates the user for a verified signup_token and signs
// them in. The account is created already email-verified, with the moment
// of consent recorded.
func (s *Service) CompleteSignup(ctx context.Context, signupToken, firstName, lastName, password string, acceptedTerms bool, client ClientInfo) (*User, *SessionTokens, error) {
	if !acceptedTerms {
		return nil, nil, domain.ErrTermsNotAccepted
	}

	key := signupTokenKey(security.HashToken(signupToken))
	email, err := s.redis.Get(ctx, key)
	if err != nil {
		if errors.Is(err, rediscli.ErrNil) {
			return nil, nil, domain.ErrTokenExpired
		}
		return nil, nil, err
	}

	hash, err := security.HashPassword(password)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	user := &User{
		ID:              security.MustNewUUIDv4(),
		Email:           email,
		PasswordHash:    &hash,
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
		return s.recordAudit(ctx, nil, &user.ID, nil, audit.ActionUserRegistered,
			map[string]any{"via": "email_otp", "ip": client.IP})
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
