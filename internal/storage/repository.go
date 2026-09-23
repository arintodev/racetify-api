package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// Repository is tenant-scoped for everything an authenticated caller does
// (RequestUpload/RequestDownload/List, all via WithTenantTx - see
// Service), except MarkStored and GetByKeyUnscoped, which run on the
// BYPASSRLS admin connection. Those two exist for exactly one reason: the
// PUT/GET object endpoints in this package's handler.go are, by design,
// unauthenticated at the HTTP layer - a presigned URL's signature IS the
// authorization (the same "possession of an unguessable secret" pattern
// internal/tenant's InvitationRepository.GetByTokenHash and
// internal/oauthclient's Repository.GetByClientID already use), so there
// is no user session to open a tenant-scoped transaction from. tenant_id
// is instead taken from the signed URL itself (which cannot be forged
// without the presign secret - see objectstorage.Store.VerifySignature),
// and used as an explicit WHERE clause on the admin connection rather than
// relied on via RLS.
type Repository struct {
	db      *database.DB
	adminDB *database.DB
}

func NewRepository(db, adminDB *database.DB) *Repository {
	return &Repository{db: db, adminDB: adminDB}
}

const objectColumns = `id, tenant_id, bucket, object_key, content_type, size_bytes, sha256, status, created_by, created_at, updated_at`

// Upsert creates a new 'pending' object row, or resets an existing one
// back to 'pending' if the same (tenant, bucket, key) was already
// uploaded before (see 0004_object_storage.up.sql's unique constraint doc
// comment: a re-upload reuses the row rather than accumulating orphans).
func (r *Repository) Upsert(ctx context.Context, o *Object) error {
	row := r.db.Q(ctx).QueryRowContext(ctx, `
		INSERT INTO objects (id, tenant_id, bucket, object_key, content_type, size_bytes, sha256, status, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 0, NULL, 'pending', $6, $7, $7)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			content_type = EXCLUDED.content_type,
			size_bytes = 0,
			sha256 = NULL,
			status = 'pending',
			created_by = EXCLUDED.created_by,
			updated_at = EXCLUDED.updated_at
		RETURNING id`,
		o.ID, o.TenantID, o.Bucket, o.ObjectKey, o.ContentType, o.CreatedBy, o.CreatedAt,
	)
	return row.Scan(&o.ID)
}

// MarkStored transitions a pending object to 'stored' once the PUT
// endpoint has finished writing and decrypting the bytes successfully.
// Runs on the admin (BYPASSRLS) connection - see the type doc comment for
// why - filtered explicitly by tenant_id/bucket/object_key rather than
// relying on RLS. sha256Hex is a pointer (nil stores SQL NULL) because not
// every caller can compute a plaintext hash - see MarkStoredTenantScoped's
// doc comment for the case that can't.
func (r *Repository) MarkStored(ctx context.Context, tenantID string, bucket ObjectBucket, key string, sha256Hex *string, sizeBytes int64, at time.Time) error {
	res, err := r.adminDB.DB.ExecContext(ctx, `
		UPDATE objects SET status = 'stored', sha256 = $4, size_bytes = $5, updated_at = $6
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key, sha256Hex, sizeBytes, at,
	)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// MarkStoredTenantScoped is MarkStored's tenant-scoped counterpart, used by
// Service.CompleteUpload (the "r2" driver's confirm-upload flow). Unlike
// the unauthenticated PUT handler that calls MarkStored, the confirm-
// upload endpoint runs behind a normal authenticated tenant session, so it
// goes through the ordinary RLS-scoped connection like every other
// authenticated write in this codebase, instead of the admin/BYPASSRLS
// connection MarkStored needs. sha256Hex is always nil in practice here:
// the "r2" driver's ConfirmUpload only HEADs the object (it never
// downloads the bytes back through this server - that would defeat the
// point of a direct-to-bucket upload), so there is no plaintext to hash -
// see r2Driver.ConfirmUpload's doc comment. The sha256 column stays
// nullable (0004_object_storage.up.sql) precisely for this case.
func (r *Repository) MarkStoredTenantScoped(ctx context.Context, tenantID string, bucket ObjectBucket, key string, sha256Hex *string, sizeBytes int64, at time.Time) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE objects SET status = 'stored', sha256 = $4, size_bytes = $5, updated_at = $6
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key, sha256Hex, sizeBytes, at,
	)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// GetByKey looks up a tenant's object by (bucket, key) from within an
// already-open tenant-scoped transaction - used by Service.RequestDownload
// to confirm the object exists and belongs to the caller's own tenant
// (RLS-backed) before ever minting a presigned GET URL for it.
func (r *Repository) GetByKey(ctx context.Context, tenantID string, bucket ObjectBucket, key string) (*Object, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `
		SELECT `+objectColumns+` FROM objects WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key)
	return scanObject(row)
}

// GetByKeyUnscoped is GetByKey's admin-connection counterpart, used by the
// unauthenticated GET object endpoint to resolve content_type before
// streaming decrypted bytes back - see the type doc comment for why this
// is safe (the caller already proved possession of a valid signature for
// this exact tenant_id/bucket/key before this is ever called).
func (r *Repository) GetByKeyUnscoped(ctx context.Context, tenantID string, bucket ObjectBucket, key string) (*Object, error) {
	row := r.adminDB.DB.QueryRowContext(ctx, `
		SELECT `+objectColumns+` FROM objects WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key)
	return scanObject(row)
}

// List returns a tenant's objects newest-first, keyset-paginated by
// (created_at, id) - see internal/platform/pagination's doc comment for why.
func (r *Repository) List(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[Object], error) {
	limit := page.NormalizeLimit()
	query := `SELECT ` + objectColumns + ` FROM objects WHERE tenant_id = $1`
	args := []any{tenantID}

	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		query += ` AND (created_at, id) < ($2, $3)`
		args = append(args, c.CreatedAt, c.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprint(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[Object]{}, err
	}
	defer rows.Close()

	var out []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			return pagination.Page[Object]{}, err
		}
		out = append(out, *o)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[Object]{}, err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return pagination.Page[Object]{Items: out, NextCursor: next}, nil
}

func scanObject(row dbutil.RowScanner) (*Object, error) {
	o := &Object{}
	err := row.Scan(
		&o.ID, &o.TenantID, &o.Bucket, &o.ObjectKey, &o.ContentType, &o.SizeBytes, &o.SHA256,
		&o.Status, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return o, nil
}
