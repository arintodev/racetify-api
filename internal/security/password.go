// Package security implements the cryptographic primitives Racetify's auth
// system needs: password hashing, OAuth client_secret hashing, opaque
// token generation, UUIDs, and JWT issuance/verification.
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Password hashing.
//
// Deviation from the implementation guide: Fase 0 calls for Argon2id or
// bcrypt. Both of the maintained Go implementations of those
// (golang.org/x/crypto/argon2, golang.org/x/crypto/bcrypt) live under
// golang.org/x/, which this sandbox's network egress policy does not allow
// (only github.com and a handful of package registries are reachable - see
// README.md "Dependency footprint"). Rather than hand-write Argon2id's
// Blake2b-based compression function from scratch with no way to validate
// it against official test vectors here, we use PBKDF2-HMAC-SHA256
// (RFC 8018), built entirely from crypto/hmac + crypto/sha256 in the
// standard library. It is still an OWASP-endorsed password KDF (the OWASP
// Password Storage Cheat Sheet lists it as an acceptable alternative to
// Argon2id) provided the iteration count is high enough - we default to
// 210,000, in line with OWASP's 2023 guidance for PBKDF2-HMAC-SHA256.
//
// This is a deliberate, documented trade-off for the environment this code
// was authored in, not a security downgrade the team chose on the merits.
// Swapping to Argon2id later only touches this file: HashPassword/
// VerifyPassword's signatures do not need to change, and the stored format
// already versions itself (`pbkdf2-sha256$...`) so existing hashes keep
// verifying against the old scheme while new ones can use a new prefix.
const (
	pbkdf2Algo       = "pbkdf2-sha256"
	pbkdf2Iterations = 210_000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 32
)

// HashPassword derives a salted PBKDF2-HMAC-SHA256 hash and encodes it as
// `pbkdf2-sha256$<iterations>$<base64-salt>$<base64-hash>` so the
// iteration count and salt travel with the hash (PHC-string-like, without
// pulling in a PHC parsing dependency).
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("security: password must not be empty")
	}

	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("security: generate salt: %w", err)
	}

	derived := pbkdf2Sha256(password, salt, pbkdf2Iterations, pbkdf2KeyLen)

	encoded := fmt.Sprintf("%s$%d$%s$%s",
		pbkdf2Algo,
		pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(derived),
	)
	return encoded, nil
}

// VerifyPassword checks password against a hash produced by HashPassword
// using a constant-time comparison to avoid timing side channels.
func VerifyPassword(password, encodedHash string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Algo {
		return false, fmt.Errorf("security: unrecognized password hash format")
	}

	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false, fmt.Errorf("security: invalid iteration count in hash")
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, fmt.Errorf("security: invalid salt encoding: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, fmt.Errorf("security: invalid hash encoding: %w", err)
	}

	got := pbkdf2Sha256(password, salt, iterations, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// pbkdf2Sha256 is RFC 8018's PBKDF2 instantiated with HMAC-SHA256, matching
// the reference algorithm used by golang.org/x/crypto/pbkdf2.
func pbkdf2Sha256(password string, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, []byte(password))
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	dk := make([]byte, 0, numBlocks*hashLen)
	buf := make([]byte, 4)

	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf)

		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)

		for i := 1; i < iterations; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range u {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}

	return dk[:keyLen]
}
