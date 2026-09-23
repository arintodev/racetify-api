-- Phase 0: tenant-scoped tables. Every table here carries a NOT NULL
-- tenant_id and gets a Row-Level Security policy in
-- 0003_row_level_security.up.sql that filters on it.
--
-- As in 0001_core.up.sql: role/status columns are plain TEXT with no CHECK
-- constraint or native enum type backing them - see that file's doc
-- comment for why.

CREATE TABLE tenant_members (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT tenant_members_tenant_user_uk UNIQUE (tenant_id, user_id)
);

CREATE TRIGGER tenant_members_set_updated_at
    BEFORE UPDATE ON tenant_members
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX tenant_members_tenant_id_idx ON tenant_members(tenant_id);
CREATE INDEX tenant_members_user_id_idx ON tenant_members(user_id);

CREATE TABLE invitations (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    email       CITEXT NOT NULL,
    role        TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,
    invited_by  UUID NOT NULL REFERENCES users(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX invitations_tenant_id_idx ON invitations(tenant_id);
CREATE INDEX invitations_email_idx ON invitations(email);

CREATE TABLE oauth_clients (
    id                  UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id           TEXT NOT NULL UNIQUE,
    client_secret_hash  TEXT NOT NULL,
    name                TEXT NOT NULL,
    scopes              TEXT[] NOT NULL DEFAULT '{}',
    status              TEXT NOT NULL DEFAULT 'active',
    created_by          UUID NOT NULL REFERENCES users(id),
    last_used_at        TIMESTAMPTZ NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER oauth_clients_set_updated_at
    BEFORE UPDATE ON oauth_clients
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX oauth_clients_tenant_id_idx ON oauth_clients(tenant_id);

-- Platform + tenant audit trail. tenant_id is nullable because some events
-- (e.g. a Runner registering) are not scoped to any tenant; see the RLS
-- policy comments in 0003 for how that nullability is handled safely.
CREATE TABLE audit_logs (
    id              UUID PRIMARY KEY,
    tenant_id       UUID NULL REFERENCES tenants(id) ON DELETE SET NULL,
    actor_user_id   UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    actor_client_id TEXT NULL,
    action          TEXT NOT NULL,
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_tenant_id_idx ON audit_logs(tenant_id);
CREATE INDEX audit_logs_action_idx ON audit_logs(action);
