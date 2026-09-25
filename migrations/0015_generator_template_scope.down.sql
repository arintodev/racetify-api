ALTER TABLE generator_templates DROP CONSTRAINT IF EXISTS generator_templates_service_ck;
DROP INDEX IF EXISTS generator_templates_scope_uk;
ALTER TABLE generator_templates
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS is_active;
