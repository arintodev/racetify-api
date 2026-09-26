package objectstorage

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"
)

// presigner implements the "local" driver's HMAC-presign scheme
// (PresignUpload, PresignDownload, PublicURL, VerifySignature) and is
// embedded by Store (local_driver.go) only - r2_driver.go uses R2/S3's own
// native SigV4 presigning instead (via *s3.PresignClient) since it issues
// URLs straight at the bucket rather than proxying through this server.
// See objectstorage.go's package doc for the ProxyDriver/DirectDriver split
// this reflects.
type presigner struct {
	secret        []byte
	publicBaseURL string
}

// sign computes the HMAC-SHA256 over exactly the fields that must not be
// tampered with: which operation, which bucket, which tenant, which key,
// and when the grant expires. Changing any one of them (including trying
// to reuse a GET signature for a PUT, or a private-bucket signature for a
// different tenant's key) invalidates the signature.
func (p *presigner) sign(method string, bucket Bucket, tenantID, key string, expiresUnix int64) string {
	mac := hmac.New(sha256.New, p.secret)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%d", method, bucket, tenantID, key, expiresUnix)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature is called by the unauthenticated PUT/GET object handlers
// (internal/storage's Handler) before touching the backend.
func (p *presigner) VerifySignature(method string, bucket Bucket, tenantID, key string, expiresUnix int64, sig string) error {
	if time.Now().Unix() > expiresUnix {
		return ErrURLExpired
	}
	want := p.sign(method, bucket, tenantID, key, expiresUnix)
	// Compare the hex strings directly (equal fixed length for any valid
	// signature) rather than decoding first - subtle.ConstantTimeCompare
	// on unequal-length byte slices would panic-free but leak length via
	// early return, and a malformed non-hex sig would fail hex.Decode
	// before reaching a timing-sensitive comparison anyway. Comparing the
	// encoded strings sidesteps both concerns.
	if len(sig) != len(want) || subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return ErrSignatureInvalid
	}
	return nil
}

const (
	http_PUT = "PUT"
	http_GET = "GET"
)

// PresignUpload returns a time-limited URL the caller can PUT raw bytes to
// (unauthenticated at the transport level - the signature IS the
// authorization, the same "possession of an unguessable secret" pattern
// already used for invitation/OAuth-client-secret tokens elsewhere in this
// codebase).
func (p *presigner) PresignUpload(bucket Bucket, tenantID, key string, ttl time.Duration) (Ticket, error) {
	return p.presign(http_PUT, bucket, tenantID, key, ttl)
}

// PresignDownload returns a time-limited GET URL for an existing object.
// Intended for the private bucket; the public bucket's PublicURL below
// needs no signature at all since it is meant to be world-readable.
func (p *presigner) PresignDownload(bucket Bucket, tenantID, key string, ttl time.Duration) (Ticket, error) {
	return p.presign(http_GET, bucket, tenantID, key, ttl)
}

func (p *presigner) presign(method string, bucket Bucket, tenantID, key string, ttl time.Duration) (Ticket, error) {
	if !bucket.Valid() {
		return Ticket{}, ErrInvalidBucket
	}
	if err := ValidateKey(key); err != nil {
		return Ticket{}, err
	}
	expiresAt := time.Now().Add(ttl)
	expiresUnix := expiresAt.Unix()
	sig := p.sign(method, bucket, tenantID, key, expiresUnix)
	return Ticket{URL: presignURL(p.publicBaseURL, bucket, tenantID, key, expiresUnix, sig), ExpiresAt: expiresAt}, nil
}

// PublicURL returns the stable, unsigned URL for a public-bucket object -
// no signature or expiry, matching the guide's "racetify-public: ...
// akses publik via CDN".
func (p *presigner) PublicURL(tenantID, key string) string {
	return p.publicBaseURL + ObjectURLPath + "/" + string(BucketPublic) + "/" + tenantID + "/" + key
}
