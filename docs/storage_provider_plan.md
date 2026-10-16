# Storage Provider Implementation Plan

## Overview
Add dynamic storage provider support for **local**, **S3**, **R2**, and future backends.  The plan adds a `provider` column to the `objects` table and wires the Go code to route reads/writes to the appropriate backend at runtime.

---

## 1.  Types & Data Structures

| File | Change | Reason |
|------|--------|--------|
| `internal/storage/object.go` | Add `Provider string` field to `Object`. | Persist provider name. |
| `internal/storage/dto.go` | Add `Provider string` to `ObjectDTO`. | API must expose provider. |
| `internal/config/config.go` | Keep `StorageDriver string` (default "local") and add a `map[string]ProviderConfig` for per‑backend configs. | Allows future backends. |
| `internal/platform/objectstorage/registry.go` | New type `ProviderRegistry` that holds a map of provider names to `Driver`. | Central driver lookup. |

---

## 2.  Repository Layer

| File | Change | Implementation details |
|------|--------|------------------------|
| `internal/storage/repository.go` | 1. Add `provider` to `objectColumns`. <br>2. Update `scanObject` to scan the provider field. | Use `row.Scan(..., &o.Provider)` with correct order. |

---

## 3.  Service Layer

| File | Change | Reason |
|------|--------|--------|
| `internal/storage/service.go` | 1. Add field `registry *ProviderRegistry`. <br>2. In constructor, create registry: `registry, _ := objectstorage.NewProviderRegistry(cfg)`. <br>3. Store provider in `ProvisionUpload`/`RequestUpload`: `obj.Provider = cfg.Storage.Driver`. <br>4. Add method `ReadObject(ctx, tenantID, objectID)` that uses the registry. | Enables routing of reads to the correct backend. |

---

## 4.  Driver Registry

Create `internal/platform/objectstorage/registry.go` (new file):

```go
package objectstorage

import "github.com/racetify/racetify-api/internal/config"

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
    local, err := NewLocalDriver(Config{
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
        r2, err := NewR2Driver(Config{
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
```

---

## 5.  Bootstrap Changes

| File | Change | Reason |
|------|--------|--------|
| `cmd/worker/main.go` | Build the registry from `cfg.Storage` and pass it to `storage.NewService`. | Makes the registry available to all workers. |

---

## 6.  Testing & Validation

1. **Unit tests** – `internal/storage/service_test.go` should create an object with local driver and read it back via `ReadObject`. 
2. **Integration tests** – Use a fake S3/R2 client (as already present in `r2_driver_test.go`) to store an object, then read it through the registry. 
3. Run `go test ./...` and `go build ./...` to ensure everything compiles.

---

## 7.  Documentation Updates

* Update `docs/phase1-api-plan.md` and the README to describe the new provider column and the registry. 
* Add examples showing how to configure `STORAGE_DRIVER` and the R2 fields in `.env`.

---

### Summary
The plan introduces a provider column, a provider registry, and a read path that selects the correct driver based on the stored provider. All existing write paths remain unchanged. This will allow the system to operate with any configured backend (local, S3, R2, etc.) without code changes after deployment.