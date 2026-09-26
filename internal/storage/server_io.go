package storage

import (
	"context"
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

func (s *Service) proxy() (objectstorage.ProxyDriver, error) {
	p, ok := s.store.(objectstorage.ProxyDriver)
	if !ok {
		return nil, fmt.Errorf("service: %w: the active storage driver does not support server-side object access", domain.ErrInvalidState)
	}
	return p, nil
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
