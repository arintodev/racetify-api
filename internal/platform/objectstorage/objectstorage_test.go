package objectstorage

import (
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func extractQueryParam(t *testing.T, rawURL, name string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url %q: %v", rawURL, err)
	}
	return u.Query().Get(name)
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(Config{
		RootDir:          t.TempDir(),
		PresignSecret:    "test-presign-secret",
		EncryptionKeyHex: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		PublicBaseURL:    "http://localhost:8080",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestValidateKeyRejectsTraversal(t *testing.T) {
	bad := []string{"", "../secret", "/abs/path", "a/../../b", "back\\slash", "\x00null"}
	for _, k := range bad {
		if err := ValidateKey(k); err == nil {
			t.Errorf("ValidateKey(%q) = nil, want an error", k)
		}
	}
	good := []string{"logo.png", "templates/bib-2026.svg", "a/b/c/d.jpg"}
	for _, k := range good {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", k, err)
		}
	}
}

func TestPresignAndVerifyRoundTrip(t *testing.T) {
	s := testStore(t)

	ticket, err := s.PresignUpload(BucketPrivate, "tenant-1", "templates/bib.svg", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if ticket.URL == "" || !strings.Contains(ticket.URL, "expires=") || !strings.Contains(ticket.URL, "sig=") {
		t.Fatalf("ticket URL missing expected query params: %s", ticket.URL)
	}

	expires := ticket.ExpiresAt.Unix()
	sig := extractQueryParam(t, ticket.URL, "sig")

	if err := s.VerifySignature("PUT", BucketPrivate, "tenant-1", "templates/bib.svg", expires, sig); err != nil {
		t.Fatalf("VerifySignature on a freshly issued ticket: %v", err)
	}
}

func TestVerifySignatureRejectsTampering(t *testing.T) {
	s := testStore(t)
	ticket, err := s.PresignUpload(BucketPrivate, "tenant-1", "a.txt", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	expires := ticket.ExpiresAt.Unix()
	sig := extractQueryParam(t, ticket.URL, "sig")

	cases := []struct {
		name        string
		method      string
		bucket      Bucket
		tenantID    string
		key         string
		expiresUnix int64
		sig         string
	}{
		{"wrong method", "GET", BucketPrivate, "tenant-1", "a.txt", expires, sig},
		{"wrong bucket", "PUT", BucketPublic, "tenant-1", "a.txt", expires, sig},
		{"wrong tenant", "PUT", BucketPrivate, "tenant-2", "a.txt", expires, sig},
		{"wrong key", "PUT", BucketPrivate, "tenant-1", "b.txt", expires, sig},
		{"tampered sig", "PUT", BucketPrivate, "tenant-1", "a.txt", expires, "00" + sig[2:]},
		{"garbage sig", "PUT", BucketPrivate, "tenant-1", "a.txt", expires, "not-hex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.VerifySignature(tc.method, tc.bucket, tc.tenantID, tc.key, tc.expiresUnix, tc.sig); err == nil {
				t.Fatalf("VerifySignature accepted a tampered/mismatched request, want an error")
			}
		})
	}
}

func TestVerifySignatureRejectsExpired(t *testing.T) {
	s := testStore(t)
	ticket, err := s.PresignUpload(BucketPrivate, "tenant-1", "a.txt", -1*time.Minute) // already expired
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	sig := extractQueryParam(t, ticket.URL, "sig")
	err = s.VerifySignature("PUT", BucketPrivate, "tenant-1", "a.txt", ticket.ExpiresAt.Unix(), sig)
	if err != ErrURLExpired {
		t.Fatalf("VerifySignature on an expired ticket = %v, want ErrURLExpired", err)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s := testStore(t)
	want := []byte("the quick brown fox jumps over the lazy dog, 032, \x00\x01\x02 binary too")

	sum, size, err := s.Put(BucketPrivate, "tenant-1", "notes/a.bin", want)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size != int64(len(want)) {
		t.Fatalf("size = %d, want %d", size, len(want))
	}
	if sum == "" {
		t.Fatalf("sha256Hex is empty")
	}

	got, err := s.Get(BucketPrivate, "tenant-1", "notes/a.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-tripped content mismatch: got %q, want %q", got, want)
	}
}

func TestGetMissingObjectReturnsNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.Get(BucketPrivate, "tenant-1", "does/not/exist.txt")
	if err != ErrNotFound {
		t.Fatalf("Get on a missing object = %v, want ErrNotFound", err)
	}
}

func TestObjectsAreEncryptedAtRest(t *testing.T) {
	s := testStore(t)
	secret := []byte("this exact plaintext must never appear on disk unencrypted")
	if _, _, err := s.Put(BucketPrivate, "tenant-1", "secret.txt", secret); err != nil {
		t.Fatalf("Put: %v", err)
	}

	path, err := s.objectPath(BucketPrivate, "tenant-1", "secret.txt")
	if err != nil {
		t.Fatalf("objectPath: %v", err)
	}
	onDisk, err := readFile(path)
	if err != nil {
		t.Fatalf("read raw file: %v", err)
	}
	if strings.Contains(string(onDisk), string(secret)) {
		t.Fatalf("plaintext was found verbatim in the on-disk file - objects must be encrypted at rest")
	}
}

func TestPresignedURLsAreTenantAndKeyScoped(t *testing.T) {
	s := testStore(t)
	ticketA, err := s.PresignDownload(BucketPrivate, "tenant-A", "photo.jpg", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignDownload: %v", err)
	}
	sigA := extractQueryParam(t, ticketA.URL, "sig")

	// A signature minted for tenant A's object must not verify for tenant
	// B's identically-named object - this is the presigned-URL-level
	// defense in depth behind cross-tenant isolation (the primary
	// guarantee is still the RLS-backed lookup the service layer does
	// before ever calling PresignDownload - see storage_service.go).
	if err := s.VerifySignature("GET", BucketPrivate, "tenant-B", "photo.jpg", ticketA.ExpiresAt.Unix(), sigA); err == nil {
		t.Fatalf("a tenant-A-scoped signature verified for tenant B - cross-tenant signature reuse must be rejected")
	}
}
