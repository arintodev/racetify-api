-- Team & loop race formats (docs/team-loop-participants-plan.md §3).
-- participants stays one row per person; teams is the new unit of
-- competition for team races and holds the official result/ranking
-- there. Race format is configured on races along two independent axes:
--
--   entry_type  - individual | team (who competes)
--   course_type - standard | relay | loop (how the distance is run)
--
-- Valid combinations, and every other rule in the plan's §4, are
-- enforced in Go (internal/event.RaceFormat.Validate for races) - enum-
-- like columns stay plain TEXT with no CHECK constraint, see
-- 0001_core.up.sql's doc comment for why.
--
-- Deliberately NOT modeled (plan §2.2): a team captain, BIB number
-- format for team races, per-leg relay distances (no race_legs table),
-- and per-lap records for loop races (no laps table - only the final lap
-- count and the time at the last counted lap are stored).

-- Existing rows default to individual/standard with every new column
-- NULL, so this is a no-op for races created before this migration.
ALTER TABLE races
    ADD COLUMN entry_type         TEXT NOT NULL DEFAULT 'individual', -- individual | team
    ADD COLUMN course_type        TEXT NOT NULL DEFAULT 'standard',   -- standard | relay | loop
    -- entry_type = team only: fixed number of members per team. One value,
    -- not a min/max range, so every team in the race competes with the
    -- same headcount.
    ADD COLUMN team_size          INTEGER NULL,
    -- course_type = loop only
    ADD COLUMN loop_mode          TEXT NULL,          -- target_laps | time_limit
    -- Length of ONE lap - not the total race distance, not cumulative.
    -- Distance covered = total_laps * loop_length_km.
    ADD COLUMN loop_length_km     NUMERIC(6,2) NULL,
    ADD COLUMN loop_target_laps   INTEGER NULL,       -- loop_mode = target_laps
    ADD COLUMN loop_time_limit_ms BIGINT NULL;        -- loop_mode = time_limit

CREATE TABLE teams (
    id               UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id         UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    race_id          UUID NOT NULL REFERENCES races(id) ON DELETE CASCADE,

    name             TEXT NOT NULL,
    gender_category  TEXT NOT NULL,                      -- male | female | mixed
    status           TEXT NOT NULL DEFAULT 'registered', -- registered | finisher | dnf | dns

    -- Whole milliseconds, same reasoning as participants' result columns
    -- (0007_participants.up.sql). relay: team finish time. loop: time at
    -- the last counted lap (finish time for target_laps, tie-break for
    -- time_limit).
    gun_time_ms      BIGINT  NULL,
    net_time_ms      BIGINT  NULL,
    -- loop only: number of counted laps.
    total_laps       INTEGER NULL,
    overall_rank     INTEGER NULL,
    -- Rank within gender_category.
    category_rank    INTEGER NULL,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT teams_race_name_uk UNIQUE (race_id, name)
);

CREATE TRIGGER teams_set_updated_at
    BEFORE UPDATE ON teams
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX teams_tenant_id_idx ON teams(tenant_id);
CREATE INDEX teams_event_id_idx ON teams(event_id);
CREATE INDEX teams_race_id_idx ON teams(race_id);

ALTER TABLE teams ENABLE ROW LEVEL SECURITY;
ALTER TABLE teams FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON teams
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The existing result columns (gun_time_ms/net_time_ms/overall_rank/
-- category_rank) keep their names; their meaning depends on the race
-- format (plan §3.3): a relay member's net_time_ms is their leg time, and
-- team members carry no rank of their own - that lives on teams.
ALTER TABLE participants
    ADD COLUMN team_id    UUID NULL REFERENCES teams(id) ON DELETE CASCADE,
    ADD COLUMN leg_order  INTEGER NULL, -- relay only: 1-based leg this person runs
    ADD COLUMN gender     TEXT NULL,    -- male | female
    ADD COLUMN total_laps INTEGER NULL; -- individual loop only

CREATE INDEX participants_team_id_idx ON participants(team_id);

-- Last name is optional (plenty of runners register with a single name).
-- The name-search index is rebuilt with coalesce(): concatenating a NULL
-- last_name would otherwise make the whole indexed expression NULL and
-- drop that runner from name search. Queries must use this exact
-- expression for the index to apply.
ALTER TABLE participants ALTER COLUMN last_name DROP NOT NULL;
DROP INDEX participants_name_lower_idx;
CREATE INDEX participants_name_lower_idx
    ON participants(lower(first_name || ' ' || coalesce(last_name, '')));
-- One member per leg within a relay team.
CREATE UNIQUE INDEX participants_team_leg_uk
    ON participants(team_id, leg_order)
    WHERE team_id IS NOT NULL AND leg_order IS NOT NULL;
