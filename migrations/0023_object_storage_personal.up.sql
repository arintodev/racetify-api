-- Personal (no-tenant) objects: a blob that belongs to a specific
-- Racetify account rather than any tenant workspace - the concrete need
-- being internal/face's self-enroll path, which uploads a face image to a
-- short-lived object just long enough for the externally-deployed
-- face-detector service to fetch it by URL, then deletes it. A Racetify
-- user's own face has no tenant to scope that temp object under (the same
-- "one identity, two independent scopes" split migrations/0022 already
-- applied to faces.tenant_id).
--
-- tenant_id becomes nullable: when NULL, the row is scoped to created_by
-- instead (RLS below). created_by also becomes nullable, for the opposite
-- pairing - a tenant-scoped object created by an OAuth M2M client
-- (internal/face's tenant ref-enrollment path) has a real tenant_id but no
-- users.id to attribute it to.
--
-- This mirrors migrations/0002's WithTenantTx mechanism with a new,
-- parallel one: db.WithUserTx sets `app.user_id` for personal-resource
-- transactions the same way WithTenantTx sets `app.tenant_id`.

ALTER TABLE objects ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE objects ALTER COLUMN created_by DROP NOT NULL;

-- objects_tenant_bucket_key_uk (tenant_id, bucket, object_key) never
-- dedupes NULL-tenant rows (SQL treats every NULL as distinct in a unique
-- index) - add the personal-scope equivalent explicitly.
CREATE UNIQUE INDEX objects_personal_bucket_key_uk ON objects (created_by, bucket, object_key) WHERE tenant_id IS NULL;

DROP POLICY tenant_isolation ON objects;
CREATE POLICY tenant_isolation ON objects
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR (tenant_id IS NULL AND created_by = NULLIF(current_setting('app.user_id', true), '')::uuid)
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR (tenant_id IS NULL AND created_by = NULLIF(current_setting('app.user_id', true), '')::uuid)
    );
