-- Phase 1: Participants (implementation_guide_phase_1.md §2/§3/§4.A). One
-- row per registered runner, ingested via CSV import (or, once results are
-- in, upserted by bib_number) rather than through Racetify's own
-- registration flow - the Wedge strategy's whole premise.
CREATE TABLE participants (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id    UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    race_id     UUID NOT NULL REFERENCES races(id) ON DELETE CASCADE,

    first_name  TEXT NOT NULL,
    last_name   TEXT NOT NULL,
    email       CITEXT NULL,
    -- Name as printed on the BIB - may differ from first/last (nickname,
    -- sponsor requirement, club name substitution).
    bib_name    TEXT NULL,
    bib_number  INTEGER NOT NULL,
    club        TEXT NULL,
    emergency_contact_name  TEXT NULL,
    emergency_contact_phone TEXT NULL,
    blood_type  TEXT NULL,

    -- registered | finisher - guide's two values; dns/dnf are a trivial
    -- follow-on, not part of Phase 1's P0 scope (docs/phase1-api-plan.md §2).
    status      TEXT NOT NULL DEFAULT 'registered',

    -- Result times stored as whole milliseconds, not INTERVAL/TEXT: this
    -- codebase's Postgres driver (lib/pq - see
    -- internal/platform/database/database.go's doc comment) has no native
    -- INTERVAL scan type, and a plain integer sorts correctly for ranking
    -- and formats trivially in Go with no driver-specific handling needed
    -- at the repository layer. NULL until the post-race results import
    -- (docs/phase1-api-plan.md §4.2, mode=results) sets them.
    gun_time_ms    BIGINT  NULL,
    net_time_ms    BIGINT  NULL,
    overall_rank   INTEGER NULL,
    category_rank  INTEGER NULL,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- DB-level backstop for "sistem harus bisa mengidentifikasi jika
    -- bib_number duplikat" (implementation_guide_phase_1.md §4.A). The
    -- import job validates before insert; this constraint is what makes
    -- the upsert-by-bib_number results path (docs/phase1-api-plan.md §4.2)
    -- race-safe under concurrent imports.
    CONSTRAINT participants_event_bib_uk UNIQUE (event_id, bib_number)
);

CREATE TRIGGER participants_set_updated_at
    BEFORE UPDATE ON participants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX participants_tenant_id_idx ON participants(tenant_id);
CREATE INDEX participants_event_id_idx ON participants(event_id);
CREATE INDEX participants_race_id_idx ON participants(race_id);
-- Redundant with the UNIQUE constraint's implicit index for exact-match
-- lookups, but named explicitly since it is also what the Runner Portal's
-- BIB search (docs/phase1-api-plan.md §5) plans against.
CREATE INDEX participants_event_bib_idx ON participants(event_id, bib_number);
-- Frictionless name search (guide §4.C): case-insensitive, no pg_trgm
-- dependency needed yet - a plain lower() index lets an ILIKE '<prefix>%'
-- query use it today; swap for pg_trgm later if fuzzy/substring
-- performance ever needs it.
CREATE INDEX participants_name_lower_idx ON participants(lower(first_name || ' ' || last_name));
CREATE INDEX participants_bib_name_lower_idx ON participants(lower(bib_name)) WHERE bib_name IS NOT NULL;

ALTER TABLE participants ENABLE ROW LEVEL SECURITY;
ALTER TABLE participants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON participants
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
