-- Phase 0: core, platform-level (non tenant-scoped) tables.
--
-- `users` deliberately lives outside tenant isolation: the implementation
-- guide's "Single Runner Identity" principle means one account can join
-- many tenants, so it cannot itself carry a tenant_id.
--
-- Status/role/bucket-style columns below are plain TEXT with NO CHECK
-- constraint enumerating their legal values (and no native Postgres ENUM
-- type either). Both of those require a schema migration to add or rename
-- a value later - exactly the "gets worse when you need to add or modify
-- a value" cost this project chose not to pay. The legal set is defined
-- once, in Go (internal/domain's *Status/*Role types), which is also the
-- only place that ever writes these columns - every write in this
-- codebase goes through the service layer, never raw user input.

CREATE EXTENSION IF NOT EXISTS citext;   -- case-insensitive email comparisons
CREATE EXTENSION IF NOT EXISTS pgcrypto; -- gen_random_uuid() fallback for ad-hoc queries/seeds

-- Generic "keep updated_at fresh" trigger, reused by every table below.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE users (
    id                  UUID PRIMARY KEY,
    email               CITEXT NOT NULL UNIQUE,
    password_hash       TEXT NULL,                    -- NULL for a social-login-only account
    first_name          TEXT NOT NULL,
    last_name           TEXT NOT NULL,
    phone               TEXT NULL,
    is_email_verified   BOOLEAN NOT NULL DEFAULT false,
    is_super_admin      BOOLEAN NOT NULL DEFAULT false,
    status              TEXT NOT NULL DEFAULT 'active',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()

    -- No "must have a password or a linked social account" CHECK here: a
    -- CHECK constraint cannot reference another table (user_social_accounts,
    -- below), so that invariant is enforced the same place every status/role
    -- value is - the Go service layer (AuthService.Register always sets
    -- password_hash; the Google sign-in path creates the users row and its
    -- user_social_accounts row inside one transaction - see
    -- AuthService.LoginOrRegisterWithGoogle).
);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- External identity provider links (Google today; Apple/Facebook/etc.
-- later need a new row here, never a new column on users).
CREATE TABLE user_social_accounts (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider            TEXT NOT NULL,
    provider_user_id    TEXT NOT NULL,   -- the provider's own subject id (Google's "sub")
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT user_social_accounts_provider_uk UNIQUE (provider, provider_user_id)
);

CREATE INDEX user_social_accounts_user_id_idx ON user_social_accounts(user_id);

CREATE TABLE tenants (
    id              UUID PRIMARY KEY,
    name            TEXT NOT NULL,
    slug            TEXT NOT NULL UNIQUE,
    owner_user_id   UUID NOT NULL REFERENCES users(id),
    status          TEXT NOT NULL DEFAULT 'pending_verification',
    -- Set only for a Rejected or Suspended tenant (Platform Super Admin's
    -- "Platform Verification" step, or a later compliance action) so the
    -- Tenant Owner can be shown *why*; cleared on reactivation. See
    -- TenantService.SetTenantStatus.
    status_reason   TEXT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER tenants_set_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX tenants_owner_user_id_idx ON tenants(owner_user_id);

-- User session refresh tokens (opaque, rotated on use). Global like users:
-- a Runner's session is not tied to any one tenant.
CREATE TABLE refresh_tokens (
    id              UUID PRIMARY KEY,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      TEXT NOT NULL UNIQUE,
    replaced_by_hash TEXT NULL,   -- set on rotation; lets us detect refresh-token reuse
    user_agent      TEXT NULL,
    ip_address      TEXT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens(user_id);
CREATE INDEX refresh_tokens_expires_at_idx ON refresh_tokens(expires_at);

-- Single-purpose, single-use opaque tokens for email verification / magic
-- link / password reset.
CREATE TABLE email_verification_tokens (
    id          UUID PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    purpose     TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX email_verification_tokens_user_id_idx ON email_verification_tokens(user_id);
