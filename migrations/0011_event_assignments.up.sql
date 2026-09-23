-- Event-scoped Crew/Volunteer Access (docs/event-crew-access-plan.md).
-- Two tables, mirroring the existing tenant_members/invitations split
-- (0001_core.up.sql, 0002_tenant_scoped.up.sql) but scoped to one Event +
-- a narrow capability set instead of the whole tenant workspace:
--
--   event_assignments - the actual grant: user_id -> one event, with
--   capabilities (RPC check-in, participant lookup, ...), independent of
--   whether user_id is a tenant_members row anywhere at all - a pure
--   external crew/volunteer never needs one (event-crew-access-plan.md
--   §1-§2).
--
--   event_invitations - the pending offer, for when the email being
--   assigned has no Racetify account yet - the row event_assignments
--   can't represent (user_id is NOT NULL) until accepted.
--
-- Both tenant_id-scoped + RLS, policy inline in the same file that
-- creates the table, following every table added since
-- 0004_object_storage.up.sql. status columns are plain TEXT, no CHECK
-- constraint - see 0001_core.up.sql's doc comment for why the legal set
-- is enforced in Go only.
CREATE TABLE event_assignments (
    id           UUID PRIMARY KEY,
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id     UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Display-only ('crew' | 'volunteer' | 'photographer' | 'timing') -
    -- capabilities is what authorization actually checks (event-crew-
    -- access-plan.md §2.4).
    label        TEXT NOT NULL DEFAULT 'crew',
    capabilities TEXT[] NOT NULL DEFAULT '{}',
    status       TEXT NOT NULL DEFAULT 'active', -- active | revoked
    assigned_by  UUID NOT NULL REFERENCES users(id),
    expires_at   TIMESTAMPTZ NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One assignment row per (event, person) - re-assigning with new
    -- capabilities is an UPDATE, not a second row.
    CONSTRAINT event_assignments_event_user_uk UNIQUE (event_id, user_id)
);

CREATE TRIGGER event_assignments_set_updated_at
    BEFORE UPDATE ON event_assignments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX event_assignments_tenant_id_idx ON event_assignments(tenant_id);
CREATE INDEX event_assignments_event_id_idx ON event_assignments(event_id);
-- What internal/event.Repository.HasCapability (the RequireEventAccess
-- middleware's own query, internal/event/access.go) actually filters on:
-- "does this user have an active assignment on this event."
CREATE INDEX event_assignments_user_event_idx ON event_assignments(user_id, event_id) WHERE status = 'active';

ALTER TABLE event_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON event_assignments
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- event_invitations mirrors invitations (0002_tenant_scoped.up.sql)
-- column-for-column, scoped to event_id + capabilities instead of a
-- tenant-wide role.
CREATE TABLE event_invitations (
    id           UUID PRIMARY KEY,
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id     UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    email        CITEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT 'crew',
    capabilities TEXT[] NOT NULL DEFAULT '{}',
    -- expires_at is the invite TOKEN's own expiry (how long the link is
    -- valid before it must be accepted, mirroring invitations.expires_at
    -- exactly) - assignment_expires_at is a distinct, independent concept:
    -- the "berlaku sampai" the resulting event_assignments row itself
    -- should carry once accepted (event-crew-access-plan.md §5), e.g. the
    -- day the event ends, NULL for unbounded.
    token_hash            TEXT NOT NULL UNIQUE,
    invited_by            UUID NOT NULL REFERENCES users(id),
    status                TEXT NOT NULL DEFAULT 'pending', -- pending | accepted | revoked
    expires_at            TIMESTAMPTZ NOT NULL,
    assignment_expires_at TIMESTAMPTZ NULL,
    accepted_at           TIMESTAMPTZ NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX event_invitations_tenant_id_idx ON event_invitations(tenant_id);
CREATE INDEX event_invitations_event_id_idx ON event_invitations(event_id);
CREATE INDEX event_invitations_email_idx ON event_invitations(email);

ALTER TABLE event_invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON event_invitations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
