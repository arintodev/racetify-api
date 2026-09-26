-- E-certificates: one image per participant, made by the dashboard and kept
-- in Object Storage (storage_id -> objects). The certificate number is made
-- once and kept when the file is made again; it is unique per workspace so
-- the public verification link cannot be confused between two certificates.
-- template_id is set null when its template is deleted: the file stays, and
-- reads as out of date.
CREATE TABLE certificates (
    id               UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id         UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    participant_id   UUID NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    certificate_no   TEXT NOT NULL,
    template_id      UUID NULL REFERENCES generator_templates(id) ON DELETE SET NULL,
    template_version INT NULL,
    -- Fingerprint of the tag values the file was made with.
    data_hash        TEXT NULL,
    -- ready | failed
    status           TEXT NOT NULL,
    error            TEXT NULL,
    storage_id       UUID NULL REFERENCES objects(id),
    -- jpg | png
    format           TEXT NULL,
    dpi              INT NULL,
    width_px         INT NULL,
    height_px        INT NULL,
    size_bytes       BIGINT NULL,
    generated_at     TIMESTAMPTZ NULL,
    created_by       UUID NOT NULL REFERENCES users(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT certificates_status_ck CHECK (status IN ('ready', 'failed')),
    CONSTRAINT certificates_format_ck CHECK (format IS NULL OR format IN ('jpg', 'png'))
);

CREATE UNIQUE INDEX certificates_participant_uk ON certificates(participant_id);
CREATE UNIQUE INDEX certificates_no_uk ON certificates(tenant_id, certificate_no);
CREATE INDEX certificates_event_id_idx ON certificates(event_id);
CREATE INDEX certificates_template_id_idx ON certificates(template_id);

CREATE TRIGGER certificates_set_updated_at
    BEFORE UPDATE ON certificates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE certificates ENABLE ROW LEVEL SECURITY;
ALTER TABLE certificates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON certificates
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Whether an event's certificates are open to participants.
CREATE TABLE certificate_publications (
    event_id     UUID PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    published_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE certificate_publications ENABLE ROW LEVEL SECURITY;
ALTER TABLE certificate_publications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON certificate_publications
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
