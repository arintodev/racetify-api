-- Phase 1: Generator templates - BIB and E-Certificate SVG templates
-- (implementation_guide_phase_1.md §4.B). tenant_id + event_id + a
-- nullable race_id (a race-specific override; NULL means "applies to
-- every race in the event"). storage_id points at the existing Phase 0
-- `objects` table - see docs/phase1-api-plan.md §2's "Storage location
-- design" note for why an FK into objects (not inline bucket/key columns)
-- is what makes this portable across storage providers.
CREATE TABLE generator_templates (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id    UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    race_id     UUID NULL REFERENCES races(id) ON DELETE CASCADE,
    storage_id  UUID NOT NULL REFERENCES objects(id),
    name        TEXT NOT NULL,
    -- bib | certificate
    service     TEXT NOT NULL,
    -- Placeholder-field bookkeeping: which placeholder tokens this SVG
    -- uses, so the generator can validate a template before a batch job
    -- burns through every participant only to fail on a missing token.
    -- Deliberately not written with double curly braces around the word
    -- "tokens" - that collides with internal/platform/database's own
    -- migration-templating substitution syntax and made cmd/migrate fail
    -- on this file for every fresh database.
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_by  UUID NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER generator_templates_set_updated_at
    BEFORE UPDATE ON generator_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX generator_templates_tenant_id_idx ON generator_templates(tenant_id);
CREATE INDEX generator_templates_event_id_idx ON generator_templates(event_id);

ALTER TABLE generator_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE generator_templates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON generator_templates
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
