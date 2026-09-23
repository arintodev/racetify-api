package objectstorage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// r2PublicURLFallbackTTL is how long a PublicURL presigned GET is valid
// for when Config.R2.PublicBaseURL is not set - see r2Driver.PublicURL's
// doc comment. Long enough that a cached/shared link (a tenant logo in a
// web page, say) does not expire mid-session, short enough that a leaked
// link does not stay valid forever.
const r2PublicURLFallbackTTL = 7 * 24 * time.Hour

// r2Driver is the "r2" driver (a DirectDriver): real Cloudflare R2/S3
// native SigV4 presigned URLs, issued straight at the bucket - or any
// S3-compatible bucket, including real AWS S3, since R2's API is
// S3-compatible and Config.R2.Endpoint can point anywhere. Unlike the
// "local" driver (Store, local_driver.go), this server never sees the
// object bytes: no app-level AES-256-GCM encryption is applied (R2's own
// at-rest encryption is relied on instead), and a successful upload has to
// be confirmed explicitly via ConfirmUpload rather than observed by a PUT
// handler here. See objectstorage.go's package doc for the full
// local-vs-r2 contrast, and for the "not exercised against a live R2
// bucket" caveat (this package's tests, r2_driver_test.go, run it against
// fake s3PresignAPI/s3HeadAPI stand-ins instead).
type r2Driver struct {
	presign       s3PresignAPI
	head          s3HeadAPI
	bucket        string
	publicBaseURL string
}

// s3PresignAPI is the subset of *s3.PresignClient this driver calls, so
// tests can supply a fake without needing a real R2/AWS account -
// *s3.PresignClient (built by s3.NewPresignClient) satisfies this with
// zero extra code.
type s3PresignAPI interface {
	PresignPutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignGetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// s3HeadAPI is the subset of *s3.Client ConfirmUpload calls - *s3.Client
// satisfies this with zero extra code.
type s3HeadAPI interface {
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

func newR2Driver(cfg Config) (*r2Driver, error) {
	if cfg.R2.AccessKeyID == "" || cfg.R2.AccessKeySecret == "" {
		return nil, fmt.Errorf("objectstorage: R2 AccessKeyID/AccessKeySecret are required for the r2 driver")
	}
	if cfg.R2.Bucket == "" {
		return nil, fmt.Errorf("objectstorage: R2 Bucket is required for the r2 driver")
	}
	endpoint := strings.TrimRight(cfg.R2.Endpoint, "/")
	if endpoint == "" {
		if cfg.R2.AccountID == "" {
			return nil, fmt.Errorf("objectstorage: R2 AccountID (or Endpoint) is required for the r2 driver")
		}
		endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.R2.AccountID)
	}

	client := s3.New(s3.Options{
		Region:       "auto", // R2 does not use AWS regions; "auto" is Cloudflare's documented value.
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.R2.AccessKeyID, cfg.R2.AccessKeySecret, ""),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true, // R2 (like most S3-compatible stores) needs path-style addressing, not virtual-hosted-style.
	})

	return &r2Driver{
		presign:       s3.NewPresignClient(client),
		head:          client,
		bucket:        cfg.R2.Bucket,
		publicBaseURL: strings.TrimRight(cfg.R2.PublicBaseURL, "/"),
	}, nil
}

// objectKey mirrors Store.objectPath's <bucket>/<tenant_id>/<key> layout,
// but as a forward-slash S3 object key rather than a filesystem path - one
// R2 bucket holds both the public and private buckets, namespaced by this
// prefix, so setup only needs one bucket created in the Cloudflare
// dashboard (see Config.R2.Bucket's doc comment).
func objectKey(bucket Bucket, tenantID, key string) (string, error) {
	if !bucket.Valid() {
		return "", ErrInvalidBucket
	}
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return string(bucket) + "/" + tenantID + "/" + key, nil
}

// PresignUpload returns a real S3 SigV4 presigned PUT URL straight at the
// R2 bucket - the caller PUTs raw bytes directly to R2, bypassing this
// server entirely (unlike Store.PresignUpload's proxy-through-our-server
// URL). Signing is local/offline (no network call).
func (d *r2Driver) PresignUpload(bucket Bucket, tenantID, key string, ttl time.Duration) (Ticket, error) {
	objKey, err := objectKey(bucket, tenantID, key)
	if err != nil {
		return Ticket{}, err
	}
	req, err := d.presign.PresignPutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(objKey),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return Ticket{}, fmt.Errorf("objectstorage: r2 presign upload: %w", err)
	}
	return Ticket{URL: req.URL, ExpiresAt: time.Now().Add(ttl)}, nil
}

// PresignDownload returns a real S3 SigV4 presigned GET URL straight at
// the R2 bucket.
func (d *r2Driver) PresignDownload(bucket Bucket, tenantID, key string, ttl time.Duration) (Ticket, error) {
	objKey, err := objectKey(bucket, tenantID, key)
	if err != nil {
		return Ticket{}, err
	}
	req, err := d.presign.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(objKey),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return Ticket{}, fmt.Errorf("objectstorage: r2 presign download: %w", err)
	}
	return Ticket{URL: req.URL, ExpiresAt: time.Now().Add(ttl)}, nil
}

// PublicURL returns a URL for a public-bucket object with no expiry the
// caller has to manage. R2 has no default "just works" public endpoint
// without extra Cloudflare-side setup (a custom domain or an r2.dev
// subdomain mapped to the bucket), so:
//   - if Config.R2.PublicBaseURL is set (the operator has done that setup),
//     this returns a genuinely stable, unsigned URL under it;
//   - otherwise, this falls back to a long-TTL (r2PublicURLFallbackTTL)
//     presigned GET URL, so PublicURL always returns something the caller
//     can actually use without forcing that setup as a hard requirement.
//     Unlike the first case, this URL does eventually expire.
//
// Returns "" if the key is invalid or presigning fails.
func (d *r2Driver) PublicURL(tenantID, key string) string {
	objKey, err := objectKey(BucketPublic, tenantID, key)
	if err != nil {
		return ""
	}
	if d.publicBaseURL != "" {
		return d.publicBaseURL + "/" + objKey
	}
	req, err := d.presign.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(objKey),
	}, s3.WithPresignExpires(r2PublicURLFallbackTTL))
	if err != nil {
		return ""
	}
	return req.URL
}

// ConfirmUpload implements DirectDriver: it HEADs the object on R2 to
// confirm the client's direct-to-bucket PUT actually landed, and returns
// its size (no plaintext SHA-256 is available - this server never saw the
// bytes - see storage.Service.CompleteUpload for how that is represented).
// Returns ErrNotFound if nothing was ever PUT to the presigned URL.
func (d *r2Driver) ConfirmUpload(bucket Bucket, tenantID, key string) (int64, error) {
	objKey, err := objectKey(bucket, tenantID, key)
	if err != nil {
		return 0, err
	}
	out, err := d.head.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("objectstorage: r2 HeadObject: %w", err)
	}
	if out.ContentLength == nil {
		return 0, nil
	}
	return *out.ContentLength, nil
}

// isNoSuchKey recognizes a missing-object response several ways:
//   - the SDK's modeled *types.NoSuchKey (what a real S3/R2 GetObject 404
//     unmarshals into - GetObject has a body to parse an XML error from);
//   - a smithy API error whose code is "NoSuchKey" or "NotFound" - some
//     S3-compatible servers return this generic shape instead of the exact
//     modeled type;
//   - a raw *smithyhttp.ResponseError with a 404 status - what HeadObject
//     typically surfaces as, since a HEAD response has no body to parse a
//     modeled error from at all.
func isNoSuchKey(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil && respErr.Response.StatusCode == 404 {
		return true
	}
	return false
}
