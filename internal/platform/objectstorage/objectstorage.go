// Package objectstorage implements the implementation guide's §3 Object
// Storage deliverable ("Object Storage dapat menerima unggahan via API dan
// menyajikan presigned URL untuk file terenkripsi") behind a small,
// swappable Driver interface - the same "disk"/"driver" pattern Laravel's
// Storage facade (config/filesystems.php's `disks`, selected by
// FILESYSTEM_DISK) and AdonisJS's Drive (config/drive.ts's `services`,
// selected by DRIVE_DISK) use: one interface, several concrete backends,
// picked at boot by a config value (STORAGE_DRIVER - see config.go and
// .env.example) rather than a compile-time choice.
//
// Two drivers ship today, and they deliberately do NOT share one upload/
// download contract - each uses whatever is native to its backend, per an
// explicit product decision to keep "local" self-hosted-and-encrypted while
// letting "r2" behave like an ordinary R2/S3 bucket:
//
//   - "local" (local_driver.go, the default, ProxyDriver): every
//     upload/download is proxied through this API's own
//     /api/v1/storage/objects/{bucket}/{tenantID}/{key} endpoint. The
//     presigned URL is an HMAC-SHA256-signed, time-limited URL of our own
//     (presign.go's presigner) - not an S3-style signature - and the bytes
//     that pass through that endpoint are encrypted at rest with
//     AES-256-GCM (crypto.go) before they ever touch disk. Zero external
//     dependencies or credentials; the right choice for local dev or a
//     genuinely self-hosted deployment. Because our server sees every byte,
//     it can mark an object 'stored' the instant its own PUT handler
//     finishes (see storage_service.go's CompletePut) - no separate
//     confirmation step is needed.
//   - "r2" (r2_driver.go, DirectDriver): real Cloudflare R2/S3 native
//     presigned URLs (SigV4, via github.com/aws/aws-sdk-go-v2/service/s3's
//     *s3.PresignClient), pointing straight at the bucket. The client PUTs
//     or GETs the bytes directly to/from R2 - this server never sees them,
//     so there is no app-level AES-256-GCM step for this driver; R2's own
//     at-rest encryption is relied on instead. Because this server is
//     bypassed for the data itself, it cannot auto-observe a completed
//     upload the way "local" does, so the control plane adds one extra
//     step for this driver only: after PUTting to the presigned URL, the
//     client calls POST .../complete-upload (storage.Service.CompleteUpload),
//     which HEADs the object on R2 (DirectDriver.ConfirmUpload) to confirm
//     it exists and learn its size before marking it 'stored'. "r2" also
//     works against any other S3-compatible bucket, including real AWS S3,
//     by pointing STORAGE_R2_ENDPOINT at a different host.
//
// This split is why the Driver interface below only has what every backend
// can do (PresignUpload/PresignDownload/PublicURL); the proxy-only
// (VerifySignature/Put/Get) and direct-only (ConfirmUpload) methods live on
// ProxyDriver/DirectDriver instead, and callers that need them (the data-
// plane PUT/GET handlers, the confirm-upload handler) type-assert the
// active Driver to whichever narrower interface they need, returning a
// clean error when the active driver doesn't support it (e.g. calling
// complete-upload while STORAGE_DRIVER=local, or hitting the raw proxy PUT
// while STORAGE_DRIVER=r2).
//
// Why "r2" needed no special network workaround, unlike this codebase's
// Postgres/Redis clients (see internal/platform/database and
// internal/platform/rediscli's package docs for that constraint): this
// package only needs github.com/aws/aws-sdk-go-v2's source, fetched once
// at build time over git (GOPROXY=direct), not a live connection to
// Cloudflare while this repository itself was being written. Nothing in
// this sandbox has R2 credentials or reaches R2's actual endpoint, so
// r2_driver.go's presign/ConfirmUpload logic is exercised in tests only
// against fake s3PresignAPI/s3HeadAPI stand-ins (r2_driver_test.go) -
// please verify against a real R2 bucket before relying on it in
// production, the same caveat this repo's README already gives
// docker-compose.yml.
package objectstorage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Bucket mirrors the guide's §3 two-bucket split:
//   - racetify-public:  tenant logos, photo thumbnails - publicly
//     readable, meant to sit behind a CDN.
//   - racetify-private: raw participant CSVs, high-res photo originals,
//     BIB/certificate PDFs - readable only via a presigned URL.
type Bucket string

const (
	BucketPublic  Bucket = "public"
	BucketPrivate Bucket = "private"
)

func (b Bucket) Valid() bool { return b == BucketPublic || b == BucketPrivate }

var (
	ErrInvalidBucket    = errors.New("objectstorage: invalid bucket")
	ErrInvalidKey       = errors.New("objectstorage: invalid object key")
	ErrSignatureInvalid = errors.New("objectstorage: signature invalid")
	ErrURLExpired       = errors.New("objectstorage: presigned URL expired")
	ErrNotFound         = errors.New("objectstorage: object not found")
)

// Ticket is a presigned URL plus the deadline it stops working at.
type Ticket struct {
	URL       string
	ExpiresAt time.Time
}

// objectURLPath is the path the PUT/GET object endpoints are mounted at -
// shared by presign generation here and router registration in
// internal/httpapi/router.go so the two can never drift apart.
const ObjectURLPath = "/api/v1/storage/objects"

// Driver is the common surface every backend (local_driver.go's Store,
// r2_driver.go's r2Driver) implements, and the type every layer above this
// package (internal/storage's Service and Handler, internal/httpapi/
// router.go's Deps.ObjectStore) is written against - never a concrete
// driver type - so STORAGE_DRIVER can change without any of those files
// changing. It deliberately only holds
// what every driver can do; ProxyDriver and DirectDriver below hold what
// only some can (see this package's doc comment for why the split exists).
type Driver interface {
	// PresignUpload returns a time-limited URL the caller can PUT raw
	// bytes to - this API's own /api/v1/storage/objects/... endpoint for
	// "local" (ProxyDriver), a real R2/S3 SigV4 URL straight at the bucket
	// for "r2" (DirectDriver). Either way, the returned URL is what the
	// caller PUTs to; only what happens next (whether a confirm-upload
	// call is required) differs.
	PresignUpload(bucket Bucket, tenantID, key string, ttl time.Duration) (Ticket, error)
	// PresignDownload returns a time-limited GET URL for an existing
	// object.
	PresignDownload(bucket Bucket, tenantID, key string, ttl time.Duration) (Ticket, error)
	// PublicURL returns a URL for a public-bucket object with no signed
	// expiry the caller has to manage. For "local" this is a genuinely
	// stable, unsigned URL; see r2Driver.PublicURL's doc comment for why
	// the "r2" driver's version is a long-TTL presigned URL when no
	// STORAGE_R2_PUBLIC_BASE_URL is configured.
	PublicURL(tenantID, key string) string
}

// ProxyDriver is implemented only by drivers that proxy object bytes
// through this API's own server (today: local_driver.go's Store) - see
// this package's doc comment. The data-plane PUT/GET handlers
// (internal/storage's Handler) type-assert the active Driver to
// ProxyDriver and return a clean error when it doesn't satisfy this (i.e.
// STORAGE_DRIVER=r2), since those endpoints only make sense for a driver
// that observes the bytes itself.
type ProxyDriver interface {
	Driver
	// VerifySignature is called by the unauthenticated PUT/GET object
	// handlers before touching the backend.
	VerifySignature(method string, bucket Bucket, tenantID, key string, expiresUnix int64, sig string) error
	// Put encrypts data with AES-256-GCM and writes it to the backend.
	// Returns the SHA-256 of the plaintext and its size.
	Put(bucket Bucket, tenantID, key string, data []byte) (sha256Hex string, size int64, err error)
	// Get reads and decrypts an object's plaintext bytes.
	Get(bucket Bucket, tenantID, key string) ([]byte, error)
}

// DirectDriver is implemented only by drivers whose presigned URLs point
// straight at the bucket, bypassing this server for the data itself (today:
// r2_driver.go's r2Driver) - see this package's doc comment. Because this
// server never observes the PUT, it cannot auto-mark an object 'stored';
// storage.Service.CompleteUpload type-asserts the active Driver to
// DirectDriver and calls ConfirmUpload to close that gap.
type DirectDriver interface {
	Driver
	// ConfirmUpload checks that (bucket, tenantID, key) actually exists in
	// the backend (a HeadObject call for r2Driver) and returns its size.
	// Returns ErrNotFound if nothing was ever PUT to the presigned URL.
	ConfirmUpload(bucket Bucket, tenantID, key string) (size int64, err error)
}

// Config is New/NewDriver's input, mirroring config.StorageConfig
// field-for-field so callers (internal/app) can pass cfg.Storage straight
// through. Fields are grouped by which driver(s) read them; a field for a
// driver that is not selected is simply ignored.
type Config struct {
	// Driver selects the backend: "local" (default, also used when empty)
	// or "r2". See NewDriver.
	Driver string

	// PresignSecret and EncryptionKeyHex are "local"-only now (r2Driver
	// dropped both - see this package's doc comment): the HMAC-presign
	// scheme and AES-256-GCM-at-rest are specific to the proxy-through-
	// our-server design local_driver.go uses.
	//
	// PresignSecret signs upload/download URLs (HMAC-SHA256). Anyone who
	// can compute a valid signature can read/write within its (bucket,
	// tenant, key, expiry) scope, so this must be kept as secret as
	// JWTSecret.
	PresignSecret string
	// EncryptionKeyHex is a 32-byte AES-256 key, hex-encoded (64 hex
	// chars). Every object the "local" driver stores is encrypted with
	// this key before it reaches disk.
	EncryptionKeyHex string
	// PublicBaseURL prefixes the stable, unsigned URLs handed out for the
	// public bucket (tenant logos, photo thumbnails - meant to be publicly
	// cacheable per the guide's bucket split). "local"-only - this API's
	// own origin. The "r2" driver has its own R2.PublicBaseURL instead
	// (see R2Config), since it serves the public bucket straight from R2.
	PublicBaseURL string

	// RootDir is "local"-only: where encrypted object bytes are written,
	// namespaced as <RootDir>/<bucket>/<tenant_id>/<key>.
	RootDir string

	// R2 is "r2"-only.
	R2 R2Config
}

// R2Config is r2_driver.go's input. Field names are Cloudflare's own
// terms (see https://developers.cloudflare.com/r2/api/s3/tokens/) so
// .env.example's STORAGE_R2_* vars map onto them one for one.
type R2Config struct {
	// AccountID builds the default endpoint
	// (https://<AccountID>.r2.cloudflarestorage.com) when Endpoint is
	// empty. Required unless Endpoint is set directly (e.g. to point at a
	// real AWS S3 bucket, or a jurisdiction-specific R2 endpoint).
	AccountID string
	// Endpoint overrides the derived R2 endpoint. Leave empty for
	// ordinary Cloudflare R2 use; set it to use this same driver against
	// real AWS S3 or an S3-compatible provider other than R2.
	Endpoint string
	// AccessKeyID/AccessKeySecret are an R2 API token's credential pair.
	AccessKeyID     string
	AccessKeySecret string
	// Bucket is the single R2 bucket objects are namespaced within, using
	// the same <bucket>/<tenant_id>/<key> object-key scheme the "local"
	// driver uses as a filesystem path - one bucket, not two, keeps setup
	// to "create one R2 bucket" regardless of the public/private split.
	Bucket string
	// PublicBaseURL, when set, prefixes a stable, unsigned URL for
	// public-bucket objects - e.g. a custom domain or
	// pub-<hash>.r2.dev mapped to Bucket in the Cloudflare dashboard
	// (https://developers.cloudflare.com/r2/buckets/public-buckets/).
	// Left empty, r2Driver.PublicURL falls back to a long-TTL presigned
	// GET URL instead, so PublicURL always returns something usable even
	// without that extra R2-side setup - see r2Driver.PublicURL's doc
	// comment.
	PublicBaseURL string
}

// New builds the "local" driver directly (bypassing the Driver-selection
// in NewDriver). Kept as its own entry point - rather than folded into
// NewDriver - because it is also this package's test helper: tests exercise
// Store's own encrypted-at-rest guarantee via its unexported objectPath
// method (see objectstorage_test.go), which only a concrete *Store, not
// the Driver interface, exposes.
func New(cfg Config) (*Store, error) {
	return newLocalDriver(cfg)
}

// NewDriver builds whichever backend cfg.Driver names ("" and "local" both
// mean the local-disk driver; "r2" means Cloudflare R2/S3-compatible) and
// returns it as the common Driver interface - this is what
// internal/app.Build calls, so the rest of the codebase never sees which
// concrete driver is running.
func NewDriver(cfg Config) (Driver, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Driver)) {
	case "", "local":
		return New(cfg)
	case "r2":
		return newR2Driver(cfg)
	default:
		return nil, fmt.Errorf("objectstorage: unknown STORAGE_DRIVER %q (want \"local\" or \"r2\")", cfg.Driver)
	}
}

// ValidateKey rejects anything that could escape the tenant/bucket
// directory or object-key prefix it is joined into, or that is simply not
// a sane object key. Called both by the service layer (to reject bad
// input with a clean 400 before any signing happens) and defensively
// again inside each driver (belt and suspenders: a path-traversal bug
// anywhere upstream must not turn into a filesystem/bucket-key escape).
func ValidateKey(key string) error {
	if key == "" || len(key) > 512 {
		return fmt.Errorf("%w: must be 1-512 characters", ErrInvalidKey)
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "..") || strings.Contains(key, "\\") {
		return fmt.Errorf("%w: must not start with / or contain .. or \\", ErrInvalidKey)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: contains a control character", ErrInvalidKey)
		}
	}
	return nil
}

// presignURL builds the {ObjectURLPath}/{bucket}/{tenantID}/{key}?expires=&sig=
// URL every driver's presign returns - see presign.go's presigner.
func presignURL(bucket Bucket, tenantID, key string, expiresUnix int64, sig string) string {
	u := url.URL{Path: ObjectURLPath + "/" + string(bucket) + "/" + tenantID + "/" + key}
	q := u.Query()
	q.Set("expires", strconv.FormatInt(expiresUnix, 10))
	q.Set("sig", sig)
	u.RawQuery = q.Encode()
	return u.String()
}

// ReadAll is a small helper the HTTP layer uses to cap upload size before
// it ever reaches Put - see storage_handler.go's use of http.MaxBytesReader
// upstream of this.
func ReadAll(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	return buf.Bytes(), err
}
