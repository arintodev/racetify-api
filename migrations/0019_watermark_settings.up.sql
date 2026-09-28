-- Phase 1: Watermark settings. An event configures zero or more watermark
-- image layers (a logo, a sponsor mark, ...) that the gallery's thumbnail
-- job (internal/gallery/thumbnail_job.go) composites onto every generated
-- thumbnail. storage_id points at the existing Phase 0 `objects` table, the
-- same FK-into-objects shape generator_templates uses
-- (migrations/0008_generator_templates.up.sql) so the watermark image
-- itself travels through the ordinary Object Storage upload flow rather
-- than a bespoke upload endpoint.
CREATE TABLE watermarks (
    id                  UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id            UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    storage_id          UUID NOT NULL REFERENCES objects(id),
    name                TEXT NOT NULL,
    -- left | center | right
    anchor_x            TEXT NOT NULL,
    -- top | center | bottom
    anchor_y            TEXT NOT NULL,
    offset_x_percent    DOUBLE PRECISION NOT NULL,
    offset_y_percent    DOUBLE PRECISION NOT NULL,
    -- Fraction of the thumbnail canvas' diagonal the watermark's own
    -- diagonal should occupy (internal/gallery/thumbnail_job.go's
    -- applyWatermarks does the size math).
    width_percent       DOUBLE PRECISION NOT NULL,
    aspect_ratio        DOUBLE PRECISION NOT NULL,
    opacity             DOUBLE PRECISION NOT NULL DEFAULT 1,
    sort_order          INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER watermarks_set_updated_at
    BEFORE UPDATE ON watermarks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX watermarks_tenant_id_idx ON watermarks(tenant_id);
CREATE INDEX watermarks_event_id_idx ON watermarks(event_id);

ALTER TABLE watermarks ENABLE ROW LEVEL SECURITY;
ALTER TABLE watermarks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON watermarks
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
