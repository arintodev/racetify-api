package objectstorage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// encryptGCM and decryptGCM are the AES-256-GCM-at-rest half of the Driver
// contract, shared by every driver so "objects are encrypted before they
// reach the backend" is one piece of tested code (objectstorage_test.go's
// TestObjectsAreEncryptedAtRest) rather than copy-pasted per driver.
//
// Known limitation (not specific to any one driver): both hold a whole
// object in memory, since AES-GCM authenticates the entire ciphertext as
// one unit and so cannot be trivially streamed without chunking. Fine for
// Phase 0's actual file sizes (tenant logos, BIB/certificate SVG
// templates, individual photos); a very large file would need chunked
// envelope encryption first - see objectstorage.go's package doc.

// encryptGCM seals data with a random 12-byte nonce (the standard
// construction for GCM at rest), returning nonce||ciphertext ready to
// write to disk or a bucket unmodified.
func encryptGCM(aeadKey, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(aeadKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, data, nil), nil
}

// decryptGCM reverses encryptGCM: split off the leading nonce, then open
// (decrypt + authenticate) the rest.
func decryptGCM(aeadKey, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(aeadKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("objectstorage: stored object is corrupt (too short)")
	}
	nonce, sealed := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("objectstorage: decrypt object (wrong key or corrupted data): %w", err)
	}
	return plaintext, nil
}
