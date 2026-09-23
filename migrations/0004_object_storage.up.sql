-- Object Storage (implementation guide §3/§6): tracks every object handed
-- out through internal/platform/objectstorage, tenant-scoped and RLS-
-- protected exactly like tenant_members/invitations/oauth_clients
-- (0003_row_level_security.up.sql).
--
-- status distinguishes a row created by RequestUpload (a presigned PUT URL
-- was issued but the bytes may never actually arrive - a client can always
-- abandon an upload) from one confirmed by the PUT handler after the
-- bytes were written and decrypted successfully. Listing/serving only
-- ever surfaces 'stored' rows as complete; a stale 'pending' row is
-- harmless (no file exists on disk for it) and can be swept by a future
-- cleanup job - not needed for Phase 0's own deliverable, which only
-- requires that uploads work and presigned URLs are served.
--
-- bucket/status are plain TEXT with no CHECK constraint, same reasoning as
-- 0001_core.up.sql's doc comment: the legal set is enforced once, in Go
-- (internal/domain's ObjectBucket/ObjectStatus types).
CREATE TABLE objects (
    id            UUID PRIMARY KEY,
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    bucket        TEXT NOT NULL,
    object_key    TEXT NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    sha256        TEXT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    created_by    UUID NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Re-requesting an upload for the same (tenant, bucket, key) reuses the
    -- same logical object row (upsert semantics) - e.g. an organizer
    -- replacing a BIB template overwrites it rather than accumulating
    -- orphaned rows for every re-upload.
    CONSTRAINT objects_tenant_bucket_key_uk UNIQUE (tenant_id, bucket, object_key)
);

CREATE TRIGGER objects_set_updated_at
    BEFORE UPDATE ON objects
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX objects_tenant_id_idx ON objects(tenant_id);

ALTER TABLE objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE objects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON objects
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
