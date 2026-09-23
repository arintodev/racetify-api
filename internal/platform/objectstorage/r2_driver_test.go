package objectstorage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// fakePresign is an in-memory stand-in for *s3.PresignClient, implementing
// just the two methods s3PresignAPI (r2_driver.go) needs. This package's
// tests have no R2 credentials or network access to a real bucket (see
// objectstorage.go's package doc), so this is what actually exercises
// r2Driver.PresignUpload/PresignDownload/PublicURL's key construction and
// expiry handling without ever signing a real request.
type fakePresign struct {
	err error
}

func (f *fakePresign) PresignPutObject(_ context.Context, in *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	opts := s3.PresignOptions{}
	for _, fn := range optFns {
		fn(&opts)
	}
	return &v4.PresignedHTTPRequest{
		Method: http.MethodPut,
		URL:    fmt.Sprintf("https://fake-r2.example.com/%s/%s?presign=put&expires_in=%d", aws.ToString(in.Bucket), aws.ToString(in.Key), int(opts.Expires.Seconds())),
	}, nil
}

func (f *fakePresign) PresignGetObject(_ context.Context, in *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	opts := s3.PresignOptions{}
	for _, fn := range optFns {
		fn(&opts)
	}
	return &v4.PresignedHTTPRequest{
		Method: http.MethodGet,
		URL:    fmt.Sprintf("https://fake-r2.example.com/%s/%s?presign=get&expires_in=%d", aws.ToString(in.Bucket), aws.ToString(in.Key), int(opts.Expires.Seconds())),
	}, nil
}

// fakeHead is an in-memory stand-in for *s3.Client's HeadObject, so
// ConfirmUpload can be tested without a real bucket.
type fakeHead struct {
	objects map[string]int64 // "bucket/key" -> size
}

func newFakeHead() *fakeHead { return &fakeHead{objects: map[string]int64{}} }

func (f *fakeHead) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	size, ok := f.objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(size)}, nil
}

// modeledNotFoundHead always returns the SDK's fully modeled
// *types.NoSuchKey error - one of the shapes isNoSuchKey (r2_driver.go)
// recognizes, exercised separately from fakeHead's generic-API-error path.
type modeledNotFoundHead struct{}

func (modeledNotFoundHead) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return nil, &types.NoSuchKey{Message: aws.String("The specified key does not exist.")}
}

// responseErrorNotFoundHead always returns a raw *smithyhttp.ResponseError
// with a 404 status and no modeled body - the shape a real HeadObject 404
// typically takes, since a HEAD response has nothing to unmarshal an XML
// error from. This is the branch isNoSuchKey needed broadening for.
type responseErrorNotFoundHead struct{}

func (responseErrorNotFoundHead) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return nil, &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 404}},
		Err:      errors.New("not found"),
	}
}

func testR2Driver(t *testing.T) (*r2Driver, *fakePresign, *fakeHead) {
	t.Helper()
	d, err := newR2Driver(Config{
		Driver: "r2",
		R2: R2Config{
			AccountID:       "test-account",
			AccessKeyID:     "test-key-id",
			AccessKeySecret: "test-key-secret",
			Bucket:          "racetify",
		},
	})
	if err != nil {
		t.Fatalf("newR2Driver: %v", err)
	}
	presign := &fakePresign{}
	head := newFakeHead()
	d.presign = presign // swap the real *s3.PresignClient for the fake - see s3PresignAPI's doc comment.
	d.head = head       // swap the real *s3.Client for the fake - see s3HeadAPI's doc comment.
	return d, presign, head
}

func TestR2DriverRequiresConfig(t *testing.T) {
	base := Config{
		Driver: "r2",
		R2: R2Config{
			AccountID:       "acct",
			AccessKeyID:     "id",
			AccessKeySecret: "secret",
			Bucket:          "bucket",
		},
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing access key id", func(c *Config) { c.R2.AccessKeyID = "" }},
		{"missing access key secret", func(c *Config) { c.R2.AccessKeySecret = "" }},
		{"missing bucket", func(c *Config) { c.R2.Bucket = "" }},
		{"missing account id and endpoint", func(c *Config) { c.R2.AccountID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if _, err := newR2Driver(cfg); err == nil {
				t.Fatalf("newR2Driver with %s = nil error, want one", tc.name)
			}
		})
	}
}

func TestR2DriverDerivesEndpointFromAccountID(t *testing.T) {
	d, err := newR2Driver(Config{
		Driver: "r2",
		R2: R2Config{
			AccountID:       "abc123",
			AccessKeyID:     "id",
			AccessKeySecret: "secret",
			Bucket:          "bucket",
		},
	})
	if err != nil {
		t.Fatalf("newR2Driver: %v", err)
	}
	client, ok := d.head.(*s3.Client)
	if !ok {
		t.Fatalf("d.head is %T, want *s3.Client", d.head)
	}
	if got, want := client.Options().BaseEndpoint, "https://abc123.r2.cloudflarestorage.com"; got == nil || *got != want {
		t.Fatalf("BaseEndpoint = %v, want %q", got, want)
	}
}

func TestR2DriverEndpointOverride(t *testing.T) {
	d, err := newR2Driver(Config{
		Driver: "r2",
		R2: R2Config{
			Endpoint:        "https://s3.us-west-2.amazonaws.com/",
			AccessKeyID:     "id",
			AccessKeySecret: "secret",
			Bucket:          "bucket",
		},
	})
	if err != nil {
		t.Fatalf("newR2Driver: %v", err)
	}
	client, ok := d.head.(*s3.Client)
	if !ok {
		t.Fatalf("d.head is %T, want *s3.Client", d.head)
	}
	// Trailing slash must be trimmed, and AccountID is not required when
	// Endpoint is set directly.
	if got, want := client.Options().BaseEndpoint, "https://s3.us-west-2.amazonaws.com"; got == nil || *got != want {
		t.Fatalf("BaseEndpoint = %v, want %q", got, want)
	}
}

func TestR2DriverPresignUploadPointsStraightAtBucket(t *testing.T) {
	d, _, _ := testR2Driver(t)
	ticket, err := d.PresignUpload(BucketPrivate, "tenant-1", "notes/a.bin", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	// Unlike Store.PresignUpload (local_driver.go), this must NOT point at
	// this API's own /api/v1/storage/objects/... endpoint - it is a real
	// (here, faked) S3 SigV4 URL straight at the R2 bucket, carrying the
	// bucket/tenant/key-namespaced object key from objectKey.
	const want = "https://fake-r2.example.com/racetify/private/tenant-1/notes/a.bin?presign=put&expires_in=300"
	if ticket.URL != want {
		t.Fatalf("PresignUpload URL = %q, want %q", ticket.URL, want)
	}
	if ticket.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt is zero")
	}
}

func TestR2DriverPresignDownloadPointsStraightAtBucket(t *testing.T) {
	d, _, _ := testR2Driver(t)
	ticket, err := d.PresignDownload(BucketPrivate, "tenant-1", "notes/a.bin", 10*time.Minute)
	if err != nil {
		t.Fatalf("PresignDownload: %v", err)
	}
	const want = "https://fake-r2.example.com/racetify/private/tenant-1/notes/a.bin?presign=get&expires_in=600"
	if ticket.URL != want {
		t.Fatalf("PresignDownload URL = %q, want %q", ticket.URL, want)
	}
}

func TestR2DriverPresignRejectsInvalidBucketOrKey(t *testing.T) {
	d, _, _ := testR2Driver(t)
	if _, err := d.PresignUpload(Bucket("bogus"), "tenant-1", "a.txt", time.Minute); !errors.Is(err, ErrInvalidBucket) {
		t.Fatalf("PresignUpload with an invalid bucket = %v, want ErrInvalidBucket", err)
	}
	if _, err := d.PresignDownload(BucketPrivate, "tenant-1", "../escape", time.Minute); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("PresignDownload with a path-traversal key = %v, want ErrInvalidKey", err)
	}
}

func TestR2DriverPublicURLUsesConfiguredBaseWhenSet(t *testing.T) {
	d, _, _ := testR2Driver(t)
	d.publicBaseURL = "https://cdn.example.com"
	got := d.PublicURL("tenant-1", "logo.png")
	want := "https://cdn.example.com/public/tenant-1/logo.png"
	if got != want {
		t.Fatalf("PublicURL = %q, want %q", got, want)
	}
}

func TestR2DriverPublicURLFallsBackToPresignedGetWhenBaseUnset(t *testing.T) {
	d, _, _ := testR2Driver(t)
	d.publicBaseURL = "" // no STORAGE_R2_PUBLIC_BASE_URL configured
	got := d.PublicURL("tenant-1", "logo.png")
	want := fmt.Sprintf("https://fake-r2.example.com/racetify/public/tenant-1/logo.png?presign=get&expires_in=%d", int(r2PublicURLFallbackTTL.Seconds()))
	if got != want {
		t.Fatalf("PublicURL fallback = %q, want %q", got, want)
	}
}

func TestR2DriverPublicURLReturnsEmptyOnPresignError(t *testing.T) {
	d, presign, _ := testR2Driver(t)
	d.publicBaseURL = ""
	presign.err = fmt.Errorf("boom")
	if got := d.PublicURL("tenant-1", "logo.png"); got != "" {
		t.Fatalf("PublicURL on presign error = %q, want empty string", got)
	}
}

func TestR2DriverConfirmUploadReturnsSize(t *testing.T) {
	d, _, head := testR2Driver(t)
	head.objects["racetify/private/tenant-1/notes/a.bin"] = 1234

	size, err := d.ConfirmUpload(BucketPrivate, "tenant-1", "notes/a.bin")
	if err != nil {
		t.Fatalf("ConfirmUpload: %v", err)
	}
	if size != 1234 {
		t.Fatalf("size = %d, want 1234", size)
	}
}

func TestR2DriverConfirmUploadMissingObjectReturnsNotFound(t *testing.T) {
	d, _, _ := testR2Driver(t)
	_, err := d.ConfirmUpload(BucketPrivate, "tenant-1", "does/not/exist.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConfirmUpload on a missing object = %v, want ErrNotFound", err)
	}
}

func TestR2DriverConfirmUploadRecognizesModeledNoSuchKey(t *testing.T) {
	d, _, _ := testR2Driver(t)
	d.head = modeledNotFoundHead{}
	_, err := d.ConfirmUpload(BucketPrivate, "tenant-1", "anything.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConfirmUpload with a modeled *types.NoSuchKey response = %v, want ErrNotFound", err)
	}
}

func TestR2DriverConfirmUploadRecognizesUnmodeled404ResponseError(t *testing.T) {
	d, _, _ := testR2Driver(t)
	d.head = responseErrorNotFoundHead{}
	_, err := d.ConfirmUpload(BucketPrivate, "tenant-1", "anything.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConfirmUpload with an unmodeled 404 ResponseError = %v, want ErrNotFound", err)
	}
}
