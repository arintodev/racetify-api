package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/security"
)

// Server-side object access, for background jobs that read a template's files
// and write what they generate. Only drivers that keep the bytes on this
// server (objectstorage.ProxyDriver, today "local") support it; with "r2" the
// bytes never pass through here, so these return domain.ErrInvalidState.
//
// The *Direct/*Generated/*Personal methods below (StoreGeneratedPersonal,
// StoreGeneratedDirect, DeleteGenerated, DeletePersonal) are a separate,
// narrower need with a different availability story: a short-lived,
// server-generated blob that must exist behind a URL for an external
// microservice to fetch (internal/face's temp face-enrollment image,
// docs/face-tenant-enrollment-plan.md §9). Those use objectstorage.
// DirectWriter instead of ProxyDriver, which both "local" and "r2" satisfy
// - see that interface's doc comment for why r2Driver can support this one
// narrow case despite never proxying a real client upload's bytes.

func (s *Service) proxy() (objectstorage.ProxyDriver, error) {
	p, ok := s.store.(objectstorage.ProxyDriver)
	if !ok {
		return nil, fmt.Errorf("service: %w: the active storage driver does not support server-side object access", domain.ErrInvalidState)
	}
	return p, nil
}

func (s *Service) directWriter() (objectstorage.DirectWriter, error) {
	w, ok := s.store.(objectstorage.DirectWriter)
	if !ok {
		return nil, fmt.Errorf("service: %w: the active storage driver does not support direct server-side writes", domain.ErrInvalidState)
	}
	return w, nil
}

// ReadObject returns the bytes of a stored object of the tenant, by object id.
func (s *Service) ReadObject(ctx context.Context, tenantID, objectID string) ([]byte, error) {
	proxy, err := s.proxy()
	if err != nil {
		return nil, err
	}
	var obj *Object
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		obj, err = s.objects.GetByID(ctx, tenantID, objectID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if obj.Status != ObjectStatusStored {
		return nil, fmt.Errorf("service: %w: the upload has not completed yet", domain.ErrInvalidState)
	}
	data, err := proxy.Get(objectstorage.Bucket(obj.Bucket), tenantID, obj.ObjectKey)
	if err != nil {
		if errors.Is(err, objectstorage.ErrNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// StoreGenerated writes bytes the server made itself (a print file) to
// (bucket, key) and registers them as a stored object of the tenant.
func (s *Service) StoreGenerated(ctx context.Context, tenantID, actorUserID, bucketStr, key, contentType string, data []byte) (*Object, error) {
	proxy, err := s.proxy()
	if err != nil {
		return nil, err
	}
	bucket := objectstorage.Bucket(bucketStr)
	if !bucket.Valid() {
		return nil, fmt.Errorf("service: %w: bucket must be \"public\" or \"private\"", domain.ErrInvalidState)
	}
	if err := objectstorage.ValidateKey(key); err != nil {
		return nil, fmt.Errorf("service: %w: %v", domain.ErrInvalidState, err)
	}
	sum, size, err := proxy.Put(bucket, tenantID, key, data)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	obj := &Object{
		ID: security.MustNewUUIDv4(), TenantID: tenantID, Bucket: ObjectBucket(bucket),
		ObjectKey: key, ContentType: contentType, CreatedBy: actorUserID, CreatedAt: now,
	}
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.objects.Upsert(ctx, obj); err != nil {
			return err
		}
		return s.objects.MarkStoredTenantScoped(ctx, tenantID, obj.Bucket, key, &sum, size, now)
	})
	if err != nil {
		return nil, err
	}
	obj.SizeBytes, obj.Status, obj.SHA256 = size, ObjectStatusStored, &sum
	return obj, nil
}

// directPut is the shared body behind StoreGeneratedDirect/
// StoreGeneratedPersonal: writes data via DirectWriter and computes its
// SHA-256 itself (unlike ProxyDriver.Put, DirectWriter doesn't hand one
// back - r2Driver's PutObject has nothing plaintext-checksummed to return,
// since it does no app-level encryption - but the caller already holds
// data in memory here regardless of driver, so there is nothing gained by
// asking the driver to also compute it).
func (s *Service) directPut(bucketStr, subjectID, key string, data []byte) (writer objectstorage.DirectWriter, bucket objectstorage.Bucket, sha256Hex string, size int64, err error) {
	writer, err = s.directWriter()
	if err != nil {
		return nil, "", "", 0, err
	}
	bucket = objectstorage.Bucket(bucketStr)
	if !bucket.Valid() {
		return nil, "", "", 0, fmt.Errorf("service: %w: bucket must be \"public\" or \"private\"", domain.ErrInvalidState)
	}
	if err := objectstorage.ValidateKey(key); err != nil {
		return nil, "", "", 0, fmt.Errorf("service: %w: %v", domain.ErrInvalidState, err)
	}
	if err := writer.PutDirect(bucket, subjectID, key, data); err != nil {
		return nil, "", "", 0, err
	}
	sum := sha256.Sum256(data)
	return writer, bucket, hex.EncodeToString(sum[:]), int64(len(data)), nil
}

// StoreGeneratedDirect is StoreGenerated's DirectWriter-based counterpart:
// unlike StoreGenerated (ProxyDriver-only - thumbnails, certificates, ...,
// which still only work on "local"), this works on any driver that
// implements objectstorage.DirectWriter, today "local" and "r2" alike.
// Used only where the caller needs guaranteed cross-driver availability -
// internal/face's tenant M2M enrollment temp image (docs/face-tenant-
// enrollment-plan.md §9), where actorUserID is left "" (the actor is an
// OAuth client, not a Racetify account - created_by ends up SQL NULL).
func (s *Service) StoreGeneratedDirect(ctx context.Context, tenantID, actorUserID, bucketStr, key, contentType string, data []byte) (*Object, error) {
	_, bucket, sha256Hex, size, err := s.directPut(bucketStr, tenantID, key, data)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	obj := &Object{
		ID: security.MustNewUUIDv4(), TenantID: tenantID, Bucket: ObjectBucket(bucket),
		ObjectKey: key, ContentType: contentType, CreatedBy: actorUserID, CreatedAt: now,
	}
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.objects.Upsert(ctx, obj); err != nil {
			return err
		}
		return s.objects.MarkStoredTenantScoped(ctx, tenantID, obj.Bucket, key, &sha256Hex, size, now)
	})
	if err != nil {
		return nil, err
	}
	obj.SizeBytes, obj.Status, obj.SHA256 = size, ObjectStatusStored, &sha256Hex
	return obj, nil
}

// StoreGeneratedPersonal is StoreGeneratedDirect's no-tenant counterpart:
// the object is scoped to userID via created_by, inside db.WithUserTx
// rather than db.WithTenantTx. Used by internal/face's self-enroll path,
// which has no tenant to scope a tracked object under - a Racetify
// account's own face identity is global (docs/face-tenant-enrollment-
// plan.md).
func (s *Service) StoreGeneratedPersonal(ctx context.Context, userID, bucketStr, key, contentType string, data []byte) (*Object, error) {
	_, bucket, sha256Hex, size, err := s.directPut(bucketStr, userID, key, data)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	obj := &Object{
		ID: security.MustNewUUIDv4(), Bucket: ObjectBucket(bucket),
		ObjectKey: key, ContentType: contentType, CreatedBy: userID, CreatedAt: now,
	}
	err = s.db.WithUserTx(ctx, userID, func(ctx context.Context) error {
		if err := s.objects.UpsertPersonal(ctx, obj); err != nil {
			return err
		}
		return s.objects.MarkStoredPersonal(ctx, userID, obj.Bucket, key, &sha256Hex, size, now)
	})
	if err != nil {
		return nil, err
	}
	obj.SizeBytes, obj.Status, obj.SHA256 = size, ObjectStatusStored, &sha256Hex
	return obj, nil
}

// DeleteGenerated is StoreGeneratedDirect's cleanup counterpart: removes a
// tenant-scoped object's backend bytes and its tracked row. Used by
// internal/face to discard a tenant M2M enrollment's temp image once the
// externally-deployed face-detector service has fetched it.
func (s *Service) DeleteGenerated(ctx context.Context, tenantID, bucketStr, key string) error {
	writer, err := s.directWriter()
	if err != nil {
		return err
	}
	bucket := objectstorage.Bucket(bucketStr)
	if err := writer.DeleteDirect(bucket, tenantID, key); err != nil {
		return err
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.objects.DeleteTenantScoped(ctx, tenantID, ObjectBucket(bucket), key)
	})
}

// DeletePersonal is DeleteGenerated's no-tenant counterpart.
func (s *Service) DeletePersonal(ctx context.Context, userID, bucketStr, key string) error {
	writer, err := s.directWriter()
	if err != nil {
		return err
	}
	bucket := objectstorage.Bucket(bucketStr)
	if err := writer.DeleteDirect(bucket, userID, key); err != nil {
		return err
	}
	return s.db.WithUserTx(ctx, userID, func(ctx context.Context) error {
		return s.objects.DeletePersonal(ctx, userID, ObjectBucket(bucket), key)
	})
}
