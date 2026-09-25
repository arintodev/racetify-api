ALTER TABLE users DROP COLUMN IF EXISTS terms_accepted_at;
DROP INDEX IF EXISTS refresh_tokens_family_id_idx;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS origin_host;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS absolute_expires_at;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS family_id;
