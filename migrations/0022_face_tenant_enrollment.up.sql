-- Tenant face enrollment via OAuth2 Client Credentials
-- (docs/face-tenant-enrollment-plan.md). faces/face_embeddings drop their
-- event_id column entirely - event scoping for face search comes only
-- from the gallery side (photo_face_detections.photo_id -> photos.event_id),
-- which is unaffected by this migration.
--
-- A face's subject becomes exactly one of:
--   * user_id (Racetify account, self-enrolled, global - tenant_id NULL,
--     usable to search any event's gallery the account can otherwise
--     reach)
--   * ref_id  (an opaque identifier the tenant assigns to their own end
--     user - tenant_id required, scoped to that tenant only)
--
-- The old soft "revoke consent" path (consent_revoked_at, a tombstone row
-- kept after erasure) is dropped along with it: withdrawing consent now
-- means deleting the row outright (DropFace in internal/face), which also
-- frees the subject (user_id/ref_id) for re-enrollment - a tombstone
-- would have permanently blocked that via the unique indexes below.

-- ==================== faces ====================

DROP POLICY tenant_isolation ON faces;

-- CASCADE drops faces_event_id_idx, faces_tenant_event_user_uk (both
-- depend on event_id) and the column's own FK to events.
ALTER TABLE faces DROP COLUMN event_id CASCADE;
ALTER TABLE faces DROP COLUMN consent_revoked_at;
ALTER TABLE faces ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE faces ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE faces ADD COLUMN ref_id TEXT NULL;

ALTER TABLE faces ADD CONSTRAINT faces_subject_xor CHECK (
    (user_id IS NOT NULL AND ref_id IS NULL AND tenant_id IS NULL)
    OR
    (ref_id IS NOT NULL AND user_id IS NULL AND tenant_id IS NOT NULL)
);
ALTER TABLE faces ADD CONSTRAINT faces_ref_id_format CHECK (
    ref_id IS NULL OR ref_id ~ '^[A-Za-z0-9_.:-]{1,200}$'
);

-- One face per Racetify user, globally - no tenant/event in the key.
CREATE UNIQUE INDEX faces_user_uk ON faces (user_id) WHERE user_id IS NOT NULL;
-- One face per (tenant, ref_id) - not per event.
CREATE UNIQUE INDEX faces_tenant_ref_uk ON faces (tenant_id, ref_id) WHERE ref_id IS NOT NULL;

-- Looser than the usual tenant_isolation shape (see e.g. audit_logs in
-- migrations/0003): a global user-owned row (tenant_id IS NULL) must be
-- readable from *any* tenant's transaction, because a tenant's own staff
-- needs to search a Racetify user's face against that tenant's event
-- gallery. Queries that only want "my tenant's own ref_id rows" (list,
-- drop) must filter `ref_id IS NOT NULL` explicitly in SQL - this policy
-- alone will also let a global row through.
CREATE POLICY tenant_isolation ON faces
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- ==================== face_embeddings ====================

DROP POLICY tenant_isolation ON face_embeddings;

ALTER TABLE face_embeddings DROP COLUMN event_id CASCADE;
ALTER TABLE face_embeddings ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE face_embeddings ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE face_embeddings ADD COLUMN ref_id TEXT NULL;

ALTER TABLE face_embeddings ADD CONSTRAINT face_embeddings_subject_xor CHECK (
    (user_id IS NOT NULL AND ref_id IS NULL AND tenant_id IS NULL)
    OR
    (ref_id IS NOT NULL AND user_id IS NULL AND tenant_id IS NOT NULL)
);
ALTER TABLE face_embeddings ADD CONSTRAINT face_embeddings_ref_id_format CHECK (
    ref_id IS NULL OR ref_id ~ '^[A-Za-z0-9_.:-]{1,200}$'
);

CREATE POLICY tenant_isolation ON face_embeddings
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
