package storage

import (
    "context"
    "fmt"
    "time"
    "errors"

    "github.com/racetify/racetify-api/internal/audit"
    "github.com/racetify/racetify-api/internal/config"
    "github.com/racetify/racetify-api/internal/domain"
    "github.com/racetify/racetify-api/internal/platform/database"
    "github.com/racetify/racetify-api/internal/platform/objectstorage"
    "github.com/racetify/racetify-api/internal/platform/pagination"
    "github.com/racetify/racetify-api/internal/platform/rbac"
    "github.com/racetify/racetify-api/internal/security"
)

// Service implements the implementation guide's §3/§6 Object Storage
// deliverable: presigned upload/download URLs against the
// internal/platform/objectstorage backend, with every grant gated by the
// same tenant‑scoped RBAC/RLS pattern used everywhere else in this
// codebase (internal/oauthclient's Service, internal/tenant's Service).
type Service struct {
    db        *database.DB
    objects   *Repository
    audit     *audit.Repository
    store     objectstorage.Driver
    cfg       config.StorageConfig
    registry  *objectstorage.ProviderRegistry
}

// NewService creates a new Service instance.
func NewService(db *database.DB, objects *Repository, audit *audit.Repository, store objectstorage.Driver, cfg config.StorageConfig) *Service {
    registry, err := objectstorage.NewProviderRegistry(cfg)
    if err != nil {
        // Fallback to local driver if registry creation fails.
        fallbackCfg := cfg
        fallbackCfg.Driver = "local"
        fallbackCfg.R2 = config.StorageR2Config{}
        registry, err = objectstorage.NewProviderRegistry(fallbackCfg)
        if err != nil {
            panic("failed to build provider registry: " + err.Error())
        }
    }
    return &Service{db: db, objects: objects, audit: audit, store: store, cfg: cfg, registry: registry}
}

// UploadTicket is returned by RequestUpload.
type UploadTicket struct {
    ObjectID  string
    UploadURL string
    ExpiresAt time.Time
    Bucket    string
    Key       string
}

// RequestUpload provides a presigned PUT URL for the caller's tenant.
func (s *Service) RequestUpload(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, bucketStr, key, contentType string) (*UploadTicket, error) {
    if !actorRole.IsAtLeast(rbac.RoleAdmin) {
        return nil, domain.ErrForbidden
    }
    return s.ProvisionUpload(ctx, tenantID, actorUserID, bucketStr, key, contentType)
}

// ProvisionUpload creates an upload ticket without a role check.
func (s *Service) ProvisionUpload(ctx context.Context, tenantID, actorUserID, bucketStr, key, contentType string) (*UploadTicket, error) {
    bucket := objectstorage.Bucket(bucketStr)
    if !bucket.Valid() {
        return nil, fmt.Errorf("service: %w: bucket must be \"public\" or \"private\"", domain.ErrInvalidState)
    }
    if err := objectstorage.ValidateKey(key); err != nil {
        return nil, fmt.Errorf("service: %w: %v", domain.ErrInvalidState, err)
    }
    if contentType == "" {
        contentType = "application/octet-stream"
    }

    now := time.Now().UTC()
    // Create a pending object row.  We intentionally keep the struct fields
    // in the same order as the database schema to avoid accidental mismatches.
        obj := &Object{
            ID:          security.MustNewUUIDv4(),
            CreatedBy:   actorUserID,
            CreatedAt:   now,
            UpdatedAt:   now,
            TenantID:    tenantID,
            Bucket:      ObjectBucket(bucket),
            ObjectKey:   key,
            ContentType: contentType,
            Status:      ObjectStatusPending,
            Provider:    s.cfg.Driver,
        }

    err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
        if err := s.objects.Upsert(ctx, obj); err != nil {
            return err
        }
        return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionObjectUploadRequested,
            map[string]any{"bucket": bucketStr, "key": key})
    })
    if err != nil {
        return nil, err
    }

    // For the `local` driver we can use `PresignUpload`.  The caller does not
    // need to know whether the underlying driver supports direct uploads – the
    // `Driver` interface always provides `PresignUpload`.
    ticket, err := s.store.PresignUpload(bucket, tenantID, key, s.cfg.UploadTTL)
    if err != nil {
        return nil, err
    }

    return &UploadTicket{
        ObjectID:  obj.ID,
        UploadURL: ticket.URL,
        ExpiresAt: ticket.ExpiresAt,
        Bucket:    bucketStr,
        Key:       key,
    }, nil
}

// ConfirmStored checks that an object with the given ID exists for the tenant and
// returns the object metadata. It is used by the gallery layer to verify that a
// pre‑uploaded file is present before attempting to create a photo record.
//
// The method performs a tenant‑scoped transaction to ensure RLS protections.
// It returns domain.ErrNotFound if the object is missing.
// ConfirmStored returns the object with the given id once its bytes are in
// storage, without a role check (see ProvisionUpload). With the "local"
// driver the PUT handler already marked it stored; with "r2" this server
// never saw the PUT, so an object still pending is confirmed here with a
// HEAD against the bucket. An object whose upload never happened comes back
// still pending, and the caller decides what to do about it.
func (s *Service) ConfirmStored(ctx context.Context, tenantID, actorUserID, objectID string) (*Object, error) {
	var obj *Object
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		obj, err = s.objects.GetByID(ctx, tenantID, objectID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if obj.Status == ObjectStatusStored {
		return obj, nil
	}
	direct, ok := s.store.(objectstorage.DirectDriver)
	if !ok {
		return obj, nil
	}
	size, err := direct.ConfirmUpload(objectstorage.Bucket(obj.Bucket), tenantID, obj.ObjectKey)
	if err != nil {
		if errors.Is(err, objectstorage.ErrNotFound) {
			return obj, nil
		}
		return nil, err
	}
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.objects.MarkStoredTenantScoped(ctx, tenantID, obj.Bucket, obj.ObjectKey, nil, size, time.Now().UTC()); err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionObjectStored,
			map[string]any{"bucket": string(obj.Bucket), "key": obj.ObjectKey, "size_bytes": size})
	})
	if err != nil {
		return nil, err
	}
	obj.Status, obj.SizeBytes = ObjectStatusStored, size
	return obj, nil
}

// ReadObject retrieves raw bytes for an object via the provider registry.
func (s *Service) ReadObject(ctx context.Context, tenantID, objectID string) ([]byte, error) {
    var obj *Object
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		obj, err = s.objects.GetByID(ctx, tenantID, objectID)
		return err
	})
    if err != nil {
        return nil, err
    }
    driver, err := s.registry.Get(obj.Provider)
    if err != nil {
        return nil, err
    }
    // The driver should support ProxyDriver.Get for tenant-scoped reads.
    proxy, ok := driver.(objectstorage.ProxyDriver)
    if !ok {
        return nil, fmt.Errorf("service: %w: the active storage driver does not support reading objects", domain.ErrInvalidState)
    }
    return proxy.Get(objectstorage.Bucket(obj.Bucket), tenantID, obj.ObjectKey)
}

// DownloadTicket is returned by RequestDownload.
type DownloadTicket struct {
    URL       string
    ExpiresAt time.Time
}

// RequestDownload returns a download URL after validating tenant ownership.
func (s *Service) RequestDownload(ctx context.Context, tenantID, key, bucketStr string) (*DownloadTicket, error) {
    bucket := objectstorage.Bucket(bucketStr)
    if !bucket.Valid() {
        return nil, fmt.Errorf("service: %w: bucket must be \"public\" or \"private\"", domain.ErrInvalidState)
    }
    if err := objectstorage.ValidateKey(key); err != nil {
        return nil, fmt.Errorf("service: %w: %v", domain.ErrInvalidState, err)
    }

    var obj *Object
    err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
        var err error
        obj, err = s.objects.GetByKey(ctx, tenantID, ObjectBucket(bucket), key)
        return err
    })
    if err != nil {
        return nil, err
    }
    if obj.Status != ObjectStatusStored {
        return nil, fmt.Errorf("service: %w: object upload has not completed yet", domain.ErrInvalidState)
    }
    ticket, err := objectstorage.GetURL(s.store, bucket, tenantID, key, s.cfg.DownloadTTL)
    if err != nil {
        return nil, err
    }
    return &DownloadTicket{URL: ticket.URL, ExpiresAt: ticket.ExpiresAt}, nil
}

// RequestDownloadPersonal is a no‑tenant counterpart.
func (s *Service) RequestDownloadPersonal(ctx context.Context, userID, key, bucketStr string) (*DownloadTicket, error) {
    bucket := objectstorage.Bucket(bucketStr)
    if !bucket.Valid() {
        return nil, fmt.Errorf("service: %w: bucket must be \"public\" or \"private\"", domain.ErrInvalidState)
    }
    if err := objectstorage.ValidateKey(key); err != nil {
        return nil, fmt.Errorf("service: %w: %v", domain.ErrInvalidState, err)
    }
    var obj *Object
    err := s.db.WithUserTx(ctx, userID, func(ctx context.Context) error {
        var err error
        obj, err = s.objects.GetByKeyPersonal(ctx, userID, ObjectBucket(bucket), key)
        return err
    })
    if err != nil {
        return nil, err
    }
    if obj.Status != ObjectStatusStored {
        return nil, fmt.Errorf("service: %w: object upload has not completed yet", domain.ErrInvalidState)
    }
    ticket, err := objectstorage.GetURL(s.store, bucket, userID, key, s.cfg.DownloadTTL)
    if err != nil {
        return nil, err
    }
    return &DownloadTicket{URL: ticket.URL, ExpiresAt: ticket.ExpiresAt}, nil
}

// recordAudit records an audit log.
func (s *Service) recordAudit(ctx context.Context, tenantID, actorUserID, actorClientID *string, action string, metadata map[string]any) error {
    return s.audit.Record(ctx, &audit.Log{
        ID:            security.MustNewUUIDv4(),
        TenantID:      tenantID,
        ActorUserID:   actorUserID,
        ActorClientID: actorClientID,
        Action:        action,
        Metadata:      metadata,
        CreatedAt:     time.Now().UTC(),
    })
}

// CompleteUpload confirms that a client‑side upload to a direct‑to‑bucket
// driver (e.g. Cloudflare R2) has finished.  It is only applicable when the
// active driver implements `objectstorage.DirectDriver`.
//
// The method returns the size of the uploaded object so the caller can
// display progress.  On success it records the `ActionObjectStored` audit
// event.
func (s *Service) CompleteUpload(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, bucketStr, key string) (int64, error) {
    // Only the R2/S3 driver requires this confirmation step.
    if s.cfg.Driver != "r2" {
        // For local storage the object is already stored by the PUT handler.
        return 0, nil
    }
    bucket := objectstorage.Bucket(bucketStr)
    if !bucket.Valid() {
        return 0, fmt.Errorf("service: %w: bucket must be \"public\" or \"private\"", domain.ErrInvalidState)
    }
    // The active driver must support DirectDriver.
    direct, ok := s.store.(objectstorage.DirectDriver)
    if !ok {
        return 0, fmt.Errorf("service: %w: storage driver does not support confirm upload", domain.ErrInvalidState)
    }
    size, err := direct.ConfirmUpload(bucket, tenantID, key)
    if err != nil {
        return 0, err
    }
    // Record that the object is now stored.
    err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
        return s.objects.MarkStoredTenantScoped(ctx, tenantID, ObjectBucket(bucket), key, nil, size, time.Now().UTC())
    })
    if err != nil {
        return 0, err
    }
    s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionObjectStored, map[string]any{"bucket": bucketStr, "key": key})
    return size, nil
}

// CompletePut is the local‑storage counterpart to `CompleteUpload`.  It is
// called by the data‑plane PUT handler once the encrypted payload has been
// written to the backend.  The handler supplies the SHA‑256 hex value and the
// size of the plaintext; we record the object as stored.
func (s *Service) CompletePut(ctx context.Context, tenantID string, bucket objectstorage.Bucket, key string, sha256Hex string, size int64) error {
    // The data‑plane only works for the `local` driver which writes directly
    // to the database; for R2 the confirmation happens in `CompleteUpload`.
    if s.cfg.Driver == "r2" {
        return fmt.Errorf("service: %w: complete put not supported for r2 driver", domain.ErrInvalidState)
    }
    err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
        return s.objects.MarkStoredTenantScoped(ctx, tenantID, ObjectBucket(bucket), key, &sha256Hex, size, time.Now().UTC())
    })
    if err != nil {
        return err
    }
    s.recordAudit(ctx, &tenantID, nil, nil, audit.ActionObjectStored, map[string]any{"bucket": string(bucket), "key": key})
    return nil
}

// ResolveForGet returns the metadata for an object requested via the public
// presigned GET URL.  It runs on the admin connection to bypass RLS.
func (s *Service) ResolveForGet(ctx context.Context, tenantID string, bucket objectstorage.Bucket, key string) (*Object, error) {
    return s.objects.GetByKeyUnscoped(ctx, tenantID, ObjectBucket(bucket), key)
}

// ListObjects returns a paginated list of objects for a tenant.
func (s *Service) ListObjects(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[Object], error) {
    return s.objects.List(ctx, tenantID, page)
}
