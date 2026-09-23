package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// GenerateOpaqueToken returns a URL-safe random token with n bytes of
// entropy (before encoding). Used for refresh tokens, email verification /
// magic-link tokens, invitation tokens, and OAuth client_id/client_secret
// material - anywhere the implementation guide calls for an unguessable,
// single-use credential.
func GenerateOpaqueToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("security: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex-encoded SHA-256 digest of an opaque token. We
// only ever persist this digest (for refresh tokens, invitation tokens,
// email-verification tokens, and OAuth client secrets), never the raw
// value, so a database leak does not expose usable credentials - this is
// exactly the "client_secret di-hash SHA-256" requirement from the
// implementation guide, generalized to every bearer secret in the system.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SecureCompare performs a constant-time equality check on two hex/base64
// strings, for comparing a freshly computed hash against a stored one.
func SecureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// GenerateOAuthClientID produces a client_id: not secret, so it does not
// need to be hashed, but it does need to be globally unique and
// unguessable enough to not collide. Prefixed for operator readability in
// logs/dashboards (mirrors the "rtf_live_client_..." style used by Stripe
// et al.).
func GenerateOAuthClientID() (string, error) {
	token, err := GenerateOpaqueToken(18)
	if err != nil {
		return "", err
	}
	return "rtf_client_" + token, nil
}

// GenerateOAuthClientSecret produces the plaintext client_secret shown to
// the tenant exactly once at creation time. Only HashToken(secret) is ever
// stored.
func GenerateOAuthClientSecret() (string, error) {
	token, err := GenerateOpaqueToken(32)
	if err != nil {
		return "", err
	}
	return "rtf_secret_" + token, nil
}
