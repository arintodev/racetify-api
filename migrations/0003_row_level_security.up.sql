-- Phase 0: Row-Level Security. This is the database-level guarantee behind
-- the deliverable "PostgreSQL RLS berfungsi, dibuktikan dengan unit test
-- di mana 'Tenant A' tidak dapat membaca data 'Tenant B'".
--
-- How it is driven: every tenant-scoped request opens its work inside a
-- transaction and runs
--     SELECT set_config('app.tenant_id', '<uuid>', true)
-- once at the start (see internal/platform/database.DB.WithTenantTx). The
-- `true` argument scopes the setting to that transaction only (equivalent
-- to `SET LOCAL`), so it can never leak across pooled connections or
-- concurrent requests. Every policy below reads that same setting via
-- current_setting('app.tenant_id', true) - the `true` here means "return
-- NULL instead of erroring if unset", so a query issued with no tenant
-- context simply matches zero rows rather than raising.

-- Two application roles, named generically (not after this product) since
-- the name/password of each is an operational deployment detail, not
-- something the schema should hardcode - see internal/platform/database.
-- MigrationSet's doc comment for how {{APP_ROLE}} etc. below get
-- substituted from DB_APP_ROLE/DB_APP_ROLE_PASSWORD/DB_ADMIN_ROLE/
-- DB_ADMIN_ROLE_PASSWORD (internal/config.DBConfig) before this file ever
-- reaches Postgres:
--   {{APP_ROLE}}   - what the API server authenticates as day to day.
--                    Subject to RLS on every tenant table.
--   {{ADMIN_ROLE}} - reserved for genuine cross-tenant Platform Super
--                    Admin operations (organizer verification, global
--                    audit/monitoring - see the PRD's RBAC matrix). Has
--                    BYPASSRLS and must be used sparingly and never as the
--                    default connection for tenant/user-facing requests.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{{APP_ROLE}}') THEN
        CREATE ROLE {{APP_ROLE}} LOGIN PASSWORD '{{APP_ROLE_PASSWORD}}';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{{ADMIN_ROLE}}') THEN
        CREATE ROLE {{ADMIN_ROLE}} LOGIN PASSWORD '{{ADMIN_ROLE_PASSWORD}}' BYPASSRLS;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO {{APP_ROLE}}, {{ADMIN_ROLE}};
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO {{APP_ROLE}}, {{ADMIN_ROLE}};
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {{APP_ROLE}}, {{ADMIN_ROLE}};

-- FORCE (not just ENABLE) matters here: it makes the policy apply even to
-- the table owner. Whether {{APP_ROLE}} ends up owning these tables (a
-- single-role dev setup) or is merely GRANTed access to them (a
-- production setup where migrations run as a separate bootstrap owner),
-- FORCE guarantees the same enforcement either way.

ALTER TABLE tenant_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_members
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invitations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE oauth_clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_clients FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON oauth_clients
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- audit_logs allows tenant_id IS NULL rows (platform-level events, e.g. a
-- Runner registering) but only when no tenant context is active - i.e.
-- they are written/read through the same non-tenant-scoped path, never
-- visible from inside a tenant transaction and never insertable from one
-- either.
ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_logs
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
