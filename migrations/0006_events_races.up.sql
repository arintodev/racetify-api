-- Phase 1: Events & Races (implementation_guide_phase_1.md §2/§3). The
-- Wedge strategy's first tenant-owned resource - an Organizer creates an
-- Event, then one or more Races (categories/distances) under it. Both
-- tables are tenant_id-scoped + RLS, with the policy inline in the same
-- file that creates the table, following 0004_object_storage.up.sql's
-- precedent (Phase 0's 0002/0003 split schema-then-RLS across two files
-- only because RLS was being introduced for the first time there - every
-- table added since keeps its policy alongside its CREATE TABLE).
--
-- status is plain TEXT, no CHECK constraint - see 0001_core.up.sql's doc
-- comment for why the enum set is Go-only.
CREATE TABLE events (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    venue       TEXT NULL,
    start_date  DATE NULL,
    end_date    DATE NULL,
    -- Both nullable, both FKs into the existing Phase 0 objects table
    -- (private/public bucket as appropriate) - see docs/phase1-api-plan.md
    -- §2's "Storage location design" note for why an FK, not inline
    -- bucket/key columns, is what makes this portable across storage
    -- providers.
    logo_storage_id      UUID NULL REFERENCES objects(id),
    thumbnail_storage_id UUID NULL REFERENCES objects(id),
    -- draft | published | archived - the Runner Portal
    -- (docs/phase1-api-plan.md §5) only ever resolves a 'published' event.
    status      TEXT NOT NULL DEFAULT 'draft',
    created_by  UUID NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Unique per tenant, not globally - docs/phase1-api-plan.md §2 defers
    -- global uniqueness until subdomain routing (event.racetify.id)
    -- literally needs it; for now the public routes resolve the tenant via
    -- this per-tenant slug lookup (see §5).
    CONSTRAINT events_tenant_slug_uk UNIQUE (tenant_id, slug)
);

CREATE TRIGGER events_set_updated_at
    BEFORE UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX events_tenant_id_idx ON events(tenant_id);
CREATE INDEX events_status_idx ON events(status);

ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON events
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Race has no invariant of its own beyond belonging to exactly one Event -
-- the Organizer manages both together (docs/phase1-api-plan.md §9's
-- internal/event package doc). tenant_id is carried here too (rather than
-- joined through event_id) purely so RLS can filter races directly without
-- a join, matching every other tenant-scoped table in this schema.
CREATE TABLE races (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id    UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    distance_km NUMERIC(6,2) NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT races_event_slug_uk UNIQUE (event_id, slug)
);

CREATE TRIGGER races_set_updated_at
    BEFORE UPDATE ON races
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX races_tenant_id_idx ON races(tenant_id);
CREATE INDEX races_event_id_idx ON races(event_id);

ALTER TABLE races ENABLE ROW LEVEL SECURITY;
ALTER TABLE races FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON races
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
