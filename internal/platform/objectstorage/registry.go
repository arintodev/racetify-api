package objectstorage

import (
    "fmt"
    "github.com/racetify/racetify-api/internal/config"
)

// ProviderRegistry holds a map of provider names to Driver instances.
// It is created once at bootstrap from the app config.
//
// The registry only contains drivers that are actually configured.
// If a backend is not configured it simply does not appear in the map.
//
// Example usage:
//   registry, _ := NewProviderRegistry(cfg.Storage)
//   driver, _ := registry.Get("local")
//   data, _ := driver.Get(bucket, tenantID, key)
//
// The registry itself implements no logic; all concrete drivers satisfy the
// Driver interface defined in objectstorage.go.

type ProviderRegistry struct {
    drivers map[string]Driver
}

// NewProviderRegistry builds a registry from the global StorageConfig.
func NewProviderRegistry(cfg config.StorageConfig) (*ProviderRegistry, error) {
    r := &ProviderRegistry{drivers: make(map[string]Driver)}
    // Default local driver.
    if cfg.RootDir == "" {
        return nil, fmt.Errorf("StorageConfig.RootDir required for local driver")
    }
    local, err := newLocalDriver(Config{
        Driver:           "local",
        PresignSecret:   cfg.PresignSecret,
        EncryptionKeyHex: cfg.EncryptionKeyHex,
        PublicBaseURL:    cfg.PublicBaseURL,
        RootDir:          cfg.RootDir,
    })
    if err != nil {
        return nil, err
    }
    r.drivers["local"] = local

    // Optional R2 driver.
    if cfg.R2.AccessKeyID != "" && cfg.R2.AccessKeySecret != "" {
    r2, err := newR2Driver(Config{
            Driver: "r2",
            R2: R2Config{
                AccountID:       cfg.R2.AccountID,
                Endpoint:        cfg.R2.Endpoint,
                AccessKeyID:     cfg.R2.AccessKeyID,
                AccessKeySecret: cfg.R2.AccessKeySecret,
                Bucket:          cfg.R2.Bucket,
                PublicBaseURL:   cfg.R2.PublicBaseURL,
            },
        })
        if err != nil {
            return nil, err
        }
        r.drivers["r2"] = r2
    }
    return r, nil
}

func (r *ProviderRegistry) Get(name string) (Driver, error) {
    d, ok := r.drivers[name]
    if !ok {
        return nil, fmt.Errorf("unknown storage provider %q", name)
    }
    return d, nil
}
