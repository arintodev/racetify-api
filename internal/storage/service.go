package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// same tenant-scoped RBAC/RLS pattern used everywhere else in this
// codebase (internal/oauthclient's Service, internal/tenant's Service) -
// a presigned URL is never handed out for an object the caller's tenant
// does not own.
type Service struct {
	db      *database.DB
	objects *Repository
	audit   *audit.Repository
	store   objectstorage.Driver
	cfg     config.StorageConfig
}

func NewService(
	db *database.DB,
	objects *Repository,
	audit *audit.Repository,
	store objectstorage.Driver,
	cfg config.StorageConfig,
) *Service {
	return &Service{db: db, objects: objects, audit: audit, store: store, cfg: cfg}
}

// UploadTicket is what RequestUpload returns to the HTTP layer.
type UploadTicket struct {
	UploadURL string
	ExpiresAt time.Time
	Bucket    string
	Key       string
}

// RequestUpload provisions a presigned PUT URL for (bucket, key) within
// the caller's tenant. Gated at RoleAdmin+, mirroring the guide's pattern
// for other tenant-provisioning actions (invitations, OAuth clients);
// Phase 1's PRD introduces a narrower Photographer role for bulk photo
// upload specifically (racetify_prd_final.md §4's roles matrix), which
// should get its own, more limited grant here once that role exists -
// Phase 0 only needs the mechanism proven end to end.
func (s *Service) RequestUpload(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, bucketStr, key, contentType string) (*UploadTicket, error) {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return nil, domain.ErrForbidden
	}
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
	obj := &Object{
		ID: security.MustNewUUIDv4(), TenantID: tenantID, Bucket: ObjectBucket(bucket),
		ObjectKey: key, ContentType: contentType, CreatedBy: actorUserID, CreatedAt: now,
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

	ticket, err := s.store.PresignUpload(bucket, tenantID, key, s.cfg.UploadTTL)
	if err != nil {
		return nil, err
	}
	return &UploadTicket{UploadURL: ticket.URL, ExpiresAt: ticket.ExpiresAt, Bucket: bucketStr, Key: key}, nil
}

// DownloadTicket is what RequestDownload returns. For the public bucket,
// URL is a stable, unsigned link (ExpiresAt is zero); for the private
// bucket it is a time-limited presigned GET URL.
type DownloadTicket struct {
	URL       string
	ExpiresAt time.Time
}

// RequestDownload returns a way to fetch (bucket, key). Any active tenant
// member may request a download (staff need to view uploaded assets too;
// unlike RequestUpload this is read-only and does not provision anything),
// but - critically - it first confirms via a tenant-scoped, RLS-backed
// lookup that the object actually belongs to the caller's own tenant
// before minting anything: this is the primary defense behind cross-
// tenant isolation for private objects (a presigned URL's signature scope
// is the secondary, defense-in-depth layer - see
// objectstorage_test.go's TestPresignedURLsAreTenantAndKeyScoped).
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

	if bucket == objectstorage.BucketPublic {
		return &DownloadTicket{URL: s.store.PublicURL(tenantID, key)}, nil
	}
	ticket, err := s.store.PresignDownload(bucket, tenantID, key, s.cfg.DownloadTTL)
	if err != nil {
		return nil, err
	}
	return &DownloadTicket{URL: ticket.URL, ExpiresAt: ticket.ExpiresAt}, nil
}

// ListObjects returns one keyset-paginated page of tenantID's objects -
// see internal/platform/pagination for why keyset rather than
// LIMIT/OFFSET.
func (s *Service) ListObjects(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[Object], error) {
	var out pagination.Page[Object]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.objects.List(ctx, tenantID, page)
		return err
	})
	return out, err
}

// CompletePut is called by the unauthenticated PUT object HTTP handler
// (this package's handler.go) after objectstorage.Store.Put has
// successfully written and encrypted the bytes. It runs entirely on the
// admin (BYPASSRLS) connection - see Repository's type doc comment for why
// there is no tenant session to open a normal WithTenantTx from at this
// point in the request. Only meaningful for the "local" driver (a
// ProxyDriver) - see CompleteUpload for the "r2" driver's equivalent.
func (s *Service) CompletePut(ctx context.Context, tenantID string, bucket objectstorage.Bucket, key, sha256Hex string, size int64) error {
	if err := s.objects.MarkStored(ctx, tenantID, ObjectBucket(bucket), key, &sha256Hex, size, time.Now().UTC()); err != nil {
		return err
	}
	// MarkStored runs on the admin connection with no tenant context (see
	// its doc comment), so the audit write is done separately through a
	// fresh, short-lived WithTenantTx on the regular RLS-bound connection -
	// audit.Repository writes through db.Q(ctx) and requires app.tenant_id
	// to be set for a non-null-tenant row (see internal/audit). Best-effort,
	// like every other audit write in this codebase (e.g.
	// AuthService.Logout): the bytes are already safely stored, and losing
	// one audit line must never mean re-reporting a completed upload as
	// failed to the client. The original uploader's user id is not
	// available at this unauthenticated call site, so actor_user_id is
	// left nil - RequestUpload already recorded who requested it.
	_ = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.recordAudit(ctx, &tenantID, nil, nil, audit.ActionObjectStored,
			map[string]any{"bucket": string(bucket), "key": key, "size_bytes": size})
	})
	return nil
}

// CompleteUpload is the "r2" driver's counterpart to CompletePut: called by
// Handler.CompleteUpload (POST .../storage/objects/complete-upload)
// after the client has PUT bytes directly to R2 using the presigned URL
// RequestUpload issued. Unlike "local", this server never observed that
// PUT, so it cannot auto-mark the object 'stored' - the client must call
// this explicitly, and this method confirms the upload actually happened
// (via objectstorage.DirectDriver.ConfirmUpload's HeadObject check) before
// trusting it. Gated at RoleAdmin+, mirroring RequestUpload's gating -
// completing an upload is part of the same provisioning action, not a
// read. Unlike CompletePut, this runs through the ordinary tenant-scoped
// WithTenantTx: unlike the unauthenticated PUT handler, this endpoint sits
// behind a normal authenticated tenant session, so there is no need for
// Repository's admin-connection workaround - see
// Repository.MarkStoredTenantScoped's doc comment.
//
// Returns domain.ErrInvalidState if the active driver is not a
// DirectDriver (i.e. STORAGE_DRIVER=local, where this endpoint does not
// apply - CompletePut already handled it automatically), or if nothing was
// ever PUT to the presigned URL.
func (s *Service) CompleteUpload(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, bucketStr, key string) (int64, error) {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return 0, domain.ErrForbidden
	}
	bucket := objectstorage.Bucket(bucketStr)
	if !bucket.Valid() {
		return 0, fmt.Errorf("service: %w: bucket must be \"public\" or \"private\"", domain.ErrInvalidState)
	}
	if err := objectstorage.ValidateKey(key); err != nil {
		return 0, fmt.Errorf("service: %w: %v", domain.ErrInvalidState, err)
	}
	direct, ok := s.store.(objectstorage.DirectDriver)
	if !ok {
		return 0, fmt.Errorf("service: %w: the active storage driver completes uploads automatically and does not use this endpoint", domain.ErrInvalidState)
	}

	size, err := direct.ConfirmUpload(bucket, tenantID, key)
	if err != nil {
		if errors.Is(err, objectstorage.ErrNotFound) {
			return 0, fmt.Errorf("service: %w: no object was found at that bucket/key - PUT to the upload URL before confirming", domain.ErrInvalidState)
		}
		return 0, err
	}

	now := time.Now().UTC()
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.objects.MarkStoredTenantScoped(ctx, tenantID, ObjectBucket(bucket), key, nil, size, now); err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionObjectStored,
			map[string]any{"bucket": bucketStr, "key": key, "size_bytes": size})
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

// ResolveForGet is called by the unauthenticated GET object HTTP handler
// to look up an object's content_type before streaming its decrypted
// bytes back - see Repository.GetByKeyUnscoped's doc comment for why
// this runs on the admin connection.
func (s *Service) ResolveForGet(ctx context.Context, tenantID string, bucket objectstorage.Bucket, key string) (*Object, error) {
	return s.objects.GetByKeyUnscoped(ctx, tenantID, ObjectBucket(bucket), key)
}

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
