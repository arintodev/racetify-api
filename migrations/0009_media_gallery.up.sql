-- Phase 1: Media Gallery (implementation_guide_phase_1.md §4.C). Albums
-- group photos; photos carry an original (private, high-res) and a
-- thumbnail (public, watermarked) storage reference; photo_tags are the
-- OCR-detected (or manually corrected) BIB matches that drive the
-- frictionless search. All three tables tenant_id-scoped + RLS.
CREATE TABLE albums (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id    UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NULL,
    -- Only an is_public=true album's photos are ever reachable through the
    -- unauthenticated Runner Portal (docs/phase1-api-plan.md §5) - an
    -- Organizer can stage photos privately before releasing them.
    is_public   BOOLEAN NOT NULL DEFAULT false,
    created_by  UUID NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER albums_set_updated_at
    BEFORE UPDATE ON albums
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX albums_tenant_id_idx ON albums(tenant_id);
CREATE INDEX albums_event_id_idx ON albums(event_id);

ALTER TABLE albums ENABLE ROW LEVEL SECURITY;
ALTER TABLE albums FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON albums
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TABLE photos (
    id                    UUID PRIMARY KEY,
    tenant_id             UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    album_id              UUID NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    -- Private bucket, high-res, set at upload time.
    original_storage_id   UUID NOT NULL REFERENCES objects(id),
    -- Public bucket, resized + watermarked; NULL until the thumbnail job
    -- (docs/phase1-api-plan.md §6/§9's gallery/jobs/thumbnail_job.go)
    -- finishes.
    thumbnail_storage_id  UUID NULL REFERENCES objects(id),
    -- pending | processed | failed
    ocr_status            TEXT NOT NULL DEFAULT 'pending',
    created_by            UUID NOT NULL REFERENCES users(id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER photos_set_updated_at
    BEFORE UPDATE ON photos
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX photos_tenant_id_idx ON photos(tenant_id);
CREATE INDEX photos_album_id_idx ON photos(album_id);
-- "Manual Tagging/Correction" queue (implementation_guide_phase_1.md §5
-- risk mitigation): the needs-review listing filters on this directly.
CREATE INDEX photos_ocr_status_idx ON photos(ocr_status);

ALTER TABLE photos ENABLE ROW LEVEL SECURITY;
ALTER TABLE photos FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON photos
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TABLE photo_tags (
    id                UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    photo_id          UUID NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
    -- Full OCR text, e.g. "A-1024" - kept alongside bib_number so an
    -- exact-string match (guide §4.C's frictionless search clause) can
    -- match a BIB that carries a non-numeric prefix.
    bib_string        TEXT NOT NULL,
    -- Digits-only, e.g. 1024 - what most searches actually type; NULL when
    -- the OCR text has no parseable integer.
    bib_number        INTEGER NULL,
    confidence_score  NUMERIC(5,4) NULL,
    -- ocr | manual - distinguishes a machine tag from a human correction in
    -- the audit trail and in the needs-review query.
    source            TEXT NOT NULL DEFAULT 'ocr',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX photo_tags_tenant_id_idx ON photo_tags(tenant_id);
CREATE INDEX photo_tags_photo_id_idx ON photo_tags(photo_id);
-- Frictionless search's OR clause: WHERE bib_string = ? OR bib_number = ?
-- (implementation_guide_phase_1.md §4.C).
CREATE INDEX photo_tags_bib_string_idx ON photo_tags(bib_string);
CREATE INDEX photo_tags_bib_number_idx ON photo_tags(bib_number);

ALTER TABLE photo_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE photo_tags FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON photo_tags
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
