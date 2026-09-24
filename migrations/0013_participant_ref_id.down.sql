DROP INDEX IF EXISTS participants_event_ref_id_uk;
ALTER TABLE participants DROP COLUMN IF EXISTS ref_id;
