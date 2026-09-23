package security

import (
	"crypto/rand"
	"fmt"
)

// NewUUIDv4 generates a random (version 4, variant 1) UUID per RFC 4122
// using crypto/rand. It is implemented inline (rather than pulling in
// github.com/google/uuid) to keep the dependency footprint to the two
// packages that genuinely needed external code (see README.md).
func NewUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("security: generate uuid: %w", err)
	}

	// Set version (4) and variant (RFC 4122) bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// MustNewUUIDv4 panics on failure. Reserved for contexts where crypto/rand
// failing would mean the process is unusable anyway (e.g. package init).
func MustNewUUIDv4() string {
	id, err := NewUUIDv4()
	if err != nil {
		panic(err)
	}
	return id
}
