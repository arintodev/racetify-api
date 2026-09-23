// Package storage is the bounded context for the Object Storage
// deliverable: the objects table (metadata behind a file stored through
// internal/platform/objectstorage), presigned upload/download tickets, and
// the control-plane/data-plane HTTP endpoints that provision and serve
// them. It is distinct from internal/platform/objectstorage (the Driver
// interface and local/r2 implementations - how bytes actually get
// written); this package owns the "objects" table and the tenant-scoped
// RBAC/RLS rules around it, the same split that already existed between
// the pre-refactor internal/service/storage_service.go and internal/
// platform/objectstorage - see docs/phase0-refactor-plan.md §2's note on
// this non-collision.
package storage

import "time"

// ObjectBucket mirrors objectstorage.Bucket without importing the platform
// package into this file directly (kept consistent with how domain.Object
// did this before the move - see objectstorage.go's package doc for why
// the two enums are kept separate rather than reusing one type).
type ObjectBucket string

const (
	ObjectBucketPublic  ObjectBucket = "public"
	ObjectBucketPrivate ObjectBucket = "private"
)

type ObjectStatus string

const (
	ObjectStatusPending ObjectStatus = "pending"
	ObjectStatusStored  ObjectStatus = "stored"
)

// Object is one row of the objects table - the durable record behind a
// file stored through internal/platform/objectstorage. The actual bytes
// live on disk (or, after a future swap, in S3/R2); this row is the
// tenant-scoped, RLS-protected metadata that makes the object listable,
// attributable (created_by), and content-type-aware without trusting a
// client-supplied header at download time.
type Object struct {
	ID          string
	TenantID    string
	Bucket      ObjectBucket
	ObjectKey   string
	ContentType string
	SizeBytes   int64
	SHA256      *string
	Status      ObjectStatus
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
