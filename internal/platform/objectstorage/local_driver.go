package objectstorage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store is the "local" driver: presigned-URL issuance/verification
// (via the embedded *presigner) plus AES-256-GCM-encrypted read/write
// against a local root directory. It is the STORAGE_DRIVER=local (or
// unset) backend - see objectstorage.go's package doc.
type Store struct {
	*presigner
	rootDir string
	aeadKey []byte // 32 bytes, AES-256
}

func newLocalDriver(cfg Config) (*Store, error) {
	if cfg.RootDir == "" {
		return nil, fmt.Errorf("objectstorage: RootDir is required")
	}
	if cfg.PresignSecret == "" {
		return nil, fmt.Errorf("objectstorage: PresignSecret is required")
	}
	key, err := hex.DecodeString(cfg.EncryptionKeyHex)
	if err != nil {
		return nil, fmt.Errorf("objectstorage: EncryptionKeyHex is not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("objectstorage: EncryptionKeyHex must decode to 32 bytes (AES-256), got %d", len(key))
	}
	if err := os.MkdirAll(cfg.RootDir, 0o700); err != nil {
		return nil, fmt.Errorf("objectstorage: create root dir: %w", err)
	}
	return &Store{
		presigner: &presigner{secret: []byte(cfg.PresignSecret), publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/")},
		rootDir:   cfg.RootDir,
		aeadKey:   key,
	}, nil
}

// objectPath resolves the on-disk location for (bucket, tenantID, key),
// re-validating that the cleaned, joined path still lives under rootDir -
// the second half of the "belt and suspenders" traversal defense described
// on ValidateKey.
func (s *Store) objectPath(bucket Bucket, tenantID, key string) (string, error) {
	if !bucket.Valid() {
		return "", ErrInvalidBucket
	}
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	full := filepath.Join(s.rootDir, string(bucket), tenantID, filepath.FromSlash(key))
	rootAbs, err := filepath.Abs(s.rootDir)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: resolves outside storage root", ErrInvalidKey)
	}
	return fullAbs, nil
}

// Put encrypts data with AES-256-GCM and writes it atomically (temp file +
// rename, so a crash mid-write can never leave a half-written object
// visible to a concurrent Get). Returns the SHA-256 of the *plaintext* (so
// a caller can verify what they uploaded, independent of the encryption
// key) and its size.
func (s *Store) Put(bucket Bucket, tenantID, key string, data []byte) (sha256Hex string, size int64, err error) {
	path, err := s.objectPath(bucket, tenantID, key)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", 0, fmt.Errorf("objectstorage: create object dir: %w", err)
	}

	ciphertext, err := encryptGCM(s.aeadKey, data)
	if err != nil {
		return "", 0, err
	}

	tmp := path + ".tmp-" + hex.EncodeToString(ciphertext[:4])
	if err := os.WriteFile(tmp, ciphertext, 0o600); err != nil {
		return "", 0, fmt.Errorf("objectstorage: write object: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("objectstorage: finalize object: %w", err)
	}

	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

// Get decrypts and returns an object's plaintext bytes.
func (s *Store) Get(bucket Bucket, tenantID, key string) ([]byte, error) {
	path, err := s.objectPath(bucket, tenantID, key)
	if err != nil {
		return nil, err
	}
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("objectstorage: read object: %w", err)
	}
	return decryptGCM(s.aeadKey, ciphertext)
}
