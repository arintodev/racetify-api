-- Session lifecycle for the Next.js BFF integration (see the app repo's
-- docs/auth-integration-bff.md):
--
-- * family_id groups every refresh token descended from one login, so one
--   host/device session can be revoked (or its reuse punished) without
--   touching the same user's sessions on other hosts.
-- * absolute_expires_at is the hard cap (30 days from login) that idle
--   expiry (expires_at, re-derived on every rotation) can never exceed.
-- * origin_host records which frontend host the session was created for,
--   for the user's own "active sessions" view.
ALTER TABLE refresh_tokens ADD COLUMN family_id UUID NULL;
ALTER TABLE refresh_tokens ADD COLUMN absolute_expires_at TIMESTAMPTZ NULL;
ALTER TABLE refresh_tokens ADD COLUMN origin_host TEXT NULL;

-- Existing tokens each become their own single-token family.
UPDATE refresh_tokens SET family_id = id, absolute_expires_at = expires_at;

ALTER TABLE refresh_tokens ALTER COLUMN family_id SET NOT NULL;
ALTER TABLE refresh_tokens ALTER COLUMN absolute_expires_at SET NOT NULL;

CREATE INDEX refresh_tokens_family_id_idx ON refresh_tokens(family_id);

-- Consent record for the signup flow (Terms of Service + Privacy Policy).
ALTER TABLE users ADD COLUMN terms_accepted_at TIMESTAMPTZ NULL;
