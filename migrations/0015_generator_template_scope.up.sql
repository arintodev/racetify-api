-- Template scope bookkeeping the dashboard's Template menu needs on top of
-- 0008. A template is either the default of a scope (Default Event, or one
-- race), or kept but not picked automatically ("Tidak dipakai"):
--   is_active = true,  race_id NULL      -> the event default for its service
--   is_active = true,  race_id set       -> that race's own template
--   is_active = false                    -> kept, never picked automatically
-- At most one active template may hold a scope, per service.
--
-- version goes up when the design (the SVG behind storage_id) is replaced,
-- so things generated from an older version (certificates) can be told
-- apart from ones made with the current design.
ALTER TABLE generator_templates
    ADD COLUMN is_active BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN version   INTEGER NOT NULL DEFAULT 1;

CREATE UNIQUE INDEX generator_templates_scope_uk
    ON generator_templates (event_id, service, COALESCE(race_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE is_active;

ALTER TABLE generator_templates
    ADD CONSTRAINT generator_templates_service_ck CHECK (service IN ('bib', 'certificate'));
