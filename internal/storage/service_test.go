//go:build ignore
// +build ignore
package storage

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "testing"

    "github.com/racetify/racetify-api/internal/config"
    "github.com/racetify/racetify-api/internal/platform/objectstorage"
)

// mockRepository satisfies the minimal subset of Repository needed for
// ReadObject. It always returns an Object with the supplied ID and the
// provider set to "local".
type mockRepository struct{}

func (m *mockRepository) GetByID(ctx context.Context, tenantID, id string) (*Object, error) {
    return &Object{ID: id, TenantID: tenantID, Bucket: ObjectBucketPublic, ObjectKey: "foo.txt", Provider: "local"}, nil
}

func TestReadObjectWithLocalProvider(t *testing.T) {
    ctx := context.Background()
    tmpDir := t.TempDir()
    // Create a minimal storage configuration.
    cfg := config.StorageConfig{
        Driver:           "local",
        PresignSecret:   "secret",
        EncryptionKeyHex: func() string {
            // 32-byte key in hex.
            key := sha256.Sum256([]byte("devkey"))
            return hex.EncodeToString(key[:])
        }(),
        PublicBaseURL: "http://localhost",
        RootDir:       tmpDir,
    }

    // Instantiate the local driver and registry.
    // Convert to the objectstorage.Config type used by the driver factory.
    osCfg := objectstorage.Config{
        Driver:           cfg.Driver,
        RootDir:          cfg.RootDir,
        PresignSecret:   cfg.PresignSecret,
        PublicBaseURL:   cfg.PublicBaseURL,
        EncryptionKeyHex: cfg.EncryptionKeyHex,
    }
    driver, err := objectstorage.New(osCfg)
    if err != nil {
        t.Fatalf("failed to create local driver: %v", err)
    }
    _, err = objectstorage.NewProviderRegistry(cfg)
    if err != nil {
        t.Fatalf("failed to create provider registry: %v", err)
    }

    // Put a test object into the local store.
    key := "foo.txt"
    data := []byte("hello world")
    _, _, err = driver.Put(objectstorage.BucketPublic, "tenant1", key, data)
    if err != nil {
        t.Fatalf("driver.Put failed: %v", err)
    }

    // Create a registry for the local driver.
    registry, _ := objectstorage.NewProviderRegistry(cfg)
    // Build a minimal Service instance that only needs the repository and
    // registry for the ReadObject path.
    svc := &Service{objects: &mockRepository{}, registry: registry}

    // The object ID is arbitrary because the mock repo ignores it.
    got, err := svc.ReadObject(ctx, "tenant1", "obj123")
    if err != nil {
        t.Fatalf("ReadObject returned error: %v", err)
    }
    if string(got) != string(data) {
        t.Fatalf("unexpected data: got %q, want %q", string(got), string(data))
    }
}
