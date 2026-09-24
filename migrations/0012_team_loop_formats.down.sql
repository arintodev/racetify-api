DROP INDEX IF EXISTS participants_team_leg_uk;

DROP INDEX IF EXISTS participants_name_lower_idx;
UPDATE participants SET last_name = '' WHERE last_name IS NULL;
ALTER TABLE participants ALTER COLUMN last_name SET NOT NULL;
CREATE INDEX participants_name_lower_idx ON participants(lower(first_name || ' ' || last_name));
DROP INDEX IF EXISTS participants_team_id_idx;

ALTER TABLE participants
    DROP COLUMN IF EXISTS total_laps,
    DROP COLUMN IF EXISTS gender,
    DROP COLUMN IF EXISTS leg_order,
    DROP COLUMN IF EXISTS team_id;

DROP TABLE IF EXISTS teams;

ALTER TABLE races
    DROP COLUMN IF EXISTS loop_time_limit_ms,
    DROP COLUMN IF EXISTS loop_target_laps,
    DROP COLUMN IF EXISTS loop_length_km,
    DROP COLUMN IF EXISTS loop_mode,
    DROP COLUMN IF EXISTS team_size,
    DROP COLUMN IF EXISTS course_type,
    DROP COLUMN IF EXISTS entry_type;
