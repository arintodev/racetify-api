-- Face search (docs/face-search-plan.md, racetify-app repo). A face_id
-- (`faces`) is a user-enrolled identity only - it is never created by
-- clustering faces detected in gallery photos. Detected faces
-- (`photo_face_detections`) are plain, unassigned detector output: no
-- face_id column here at all, since clustering assignment is never done in
-- this schema (matching happens at search time against Qdrant, see
-- internal/platform/qdrantstore). `face_embeddings` is the enrollment log:
-- no image is ever persisted, only the resulting vector (in Qdrant) and
-- this metadata row. All three tables tenant_id-scoped + RLS, the same
-- shape as 0009_media_gallery.up.sql.

CREATE TABLE faces (
    id                  UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id            UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    embedding_count     INTEGER NOT NULL DEFAULT 0,
    -- When the user consented to storing their biometric face data for this
    -- event. Required - a faces row (and therefore any face_embeddings)
    -- cannot exist without it; the enrollment endpoint refuses the request
    -- otherwise (internal/face's consent_required error).
    consented_at        TIMESTAMPTZ NOT NULL,
    -- Set when the user withdraws consent. Search must refuse to match a
    -- face_id with this set, and revocation deletes the face's embeddings
    -- (both the face_embeddings rows and their Qdrant points).
    consent_revoked_at  TIMESTAMPTZ NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER faces_set_updated_at
    BEFORE UPDATE ON faces
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX faces_tenant_id_idx ON faces(tenant_id);
CREATE INDEX faces_event_id_idx ON faces(event_id);
CREATE INDEX faces_user_id_idx ON faces(user_id);
-- One face_id per user per event; repeated enrollment uploads add rows to
-- face_embeddings under the same face_id rather than creating a new one.
CREATE UNIQUE INDEX faces_tenant_event_user_uk ON faces (tenant_id, event_id, user_id);

ALTER TABLE faces ENABLE ROW LEVEL SECURITY;
ALTER TABLE faces FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON faces
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- One row per detected face instance in a gallery photo. Plain detector
-- output only - no face_id, no clustering, nothing assigning it to anyone.
-- The embedding vector itself lives only in Qdrant, keyed by
-- qdrant_point_id; this table never stores a vector.
CREATE TABLE photo_face_detections (
    id                UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    photo_id          UUID NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
    -- Fractions of the photo's own width/height (0.0-1.0), not pixel
    -- coordinates - same convention as photo_tags.box_x/y/w/h
    -- (migrations/0016_gallery_extend.up.sql).
    box_x             NUMERIC(5,4) NOT NULL,
    box_y             NUMERIC(5,4) NOT NULL,
    box_w             NUMERIC(5,4) NOT NULL,
    box_h             NUMERIC(5,4) NOT NULL,
    confidence_score  NUMERIC(5,4) NULL,
    qdrant_point_id   UUID NOT NULL,
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX photo_face_detections_tenant_id_idx ON photo_face_detections(tenant_id);
CREATE INDEX photo_face_detections_photo_id_idx ON photo_face_detections(photo_id);
CREATE UNIQUE INDEX photo_face_detections_qdrant_point_uk ON photo_face_detections (qdrant_point_id);

ALTER TABLE photo_face_detections ENABLE ROW LEVEL SECURITY;
ALTER TABLE photo_face_detections FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON photo_face_detections
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The multiple embeddings a user has enrolled under their face_id. No image
-- is ever persisted - the uploaded enrollment image is embedded in-memory
-- by the face-embed service and discarded once the vector is returned;
-- only the vector (in Qdrant) and this metadata row persist.
CREATE TABLE face_embeddings (
    id                UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id          UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    face_id           UUID NOT NULL REFERENCES faces(id) ON DELETE CASCADE,
    qdrant_point_id   UUID NOT NULL,
    confidence_score  NUMERIC(5,4) NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX face_embeddings_tenant_id_idx ON face_embeddings(tenant_id);
CREATE INDEX face_embeddings_face_id_idx ON face_embeddings(face_id);
CREATE INDEX face_embeddings_user_id_idx ON face_embeddings(user_id);
CREATE UNIQUE INDEX face_embeddings_qdrant_point_uk ON face_embeddings (qdrant_point_id);

ALTER TABLE face_embeddings ENABLE ROW LEVEL SECURITY;
ALTER TABLE face_embeddings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON face_embeddings
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
