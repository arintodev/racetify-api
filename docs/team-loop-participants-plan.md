# Racetify API — Team & Loop Race Formats: Participant Data (Planning)

> Planning document.
> Extends `phase1-api-plan.md` rather than replacing it: Events, Races and
> Participants (`0006_events_races`, `0007_participants`) are assumed to
> exist as designed there. Scope is **the data structure only**. API
> routes, CSV import, generator/e-certificate, Runner Portal and dashboard
> UI changes are follow-ons (§7).
>
> **Status:** migration `0012_team_loop_formats` and the race format
> configuration (§3.1, race rules in §4) are implemented in
> `internal/event` (`RaceFormat`, `POST`/`PATCH .../races`). `teams` and
> the new `participants` columns exist in the schema only, since there is
> no participant module in Go yet. Their rules in §4 are applied when
> that module is built.

## 1. Problem

Today every race is an individual race with a single finish. A
`participants` row is both the person and the unit of competition: it
holds that person's `gun_time_ms`/`net_time_ms` and their
`overall_rank`/`category_rank`.

That doesn't cover two formats organizers run:

* **Relay.** A team of runners, each running one leg in a fixed order.
  The official result is the team's total time.
* **Loop.** Runners lap a fixed circuit, in one of two modes:
  * **Target laps**: fastest to complete N laps wins.
  * **Time limit**: most laps completed within the time limit wins.

  A loop race can be individual or team-based. In a team loop, members
  take turns on the circuit, and one member may run many laps.

Teams compete in a gender category: `male`, `female` or `mixed`.

## 2. Model

### 2.1 Person vs. unit of competition

`participants` stays **one row per person**, unchanged in meaning for
everything that is per-person today (identity, emergency contact,
certificate, photo matching, Runner Portal search). A new `teams` table
becomes the unit of competition for team races, and it holds the official
result and ranking there.

Race format is set on `races` along two independent axes:

| Axis | Values | Meaning |
|---|---|---|
| `entry_type` | `individual` \| `team` | Who competes: a person or a team |
| `course_type` | `standard` \| `relay` \| `loop` | How the distance is run |

Valid combinations:

| `entry_type` | `course_type` | Format |
|---|---|---|
| `individual` | `standard` | Today's race, unchanged |
| `individual` | `loop` | Individual loop race |
| `team` | `relay` | Relay |
| `team` | `loop` | Team loop race |

`individual` + `relay` and `team` + `standard` are rejected in Go.

### 2.2 Out of scope for the model

* **Team captain.** No team has a designated captain or contact member.
* **BIB number format.** How relay/team BIBs are numbered or printed is
  not decided here. `participants.bib_number` and its
  `UNIQUE (event_id, bib_number)` constraint stay as they are.
* **Per-leg distances for relay.** Legs are identified only by their
  order within the team (`participants.leg_order`). There is no
  `race_legs` table, and per-leg distance is not modeled.
* **Per-lap records for loop races.** There is no `laps` table. Only the
  final lap count and the time at the last counted lap are stored, per
  competitor (`teams` or `participants`). Lap-by-lap splits, and which
  team member ran which lap, are not recorded.

## 3. Schema changes: migration `0012_team_loop_formats`

Following the existing convention, enum-like columns are plain `TEXT`
with no `CHECK` constraint (legal values are enforced in Go, see
`0001_core.up.sql`'s doc comment). Every new table is `tenant_id`-scoped
with its RLS policy inline, as with every table since
`0004_object_storage`.

### 3.1 `races`: format configuration

```sql
ALTER TABLE races
    ADD COLUMN entry_type          TEXT NOT NULL DEFAULT 'individual', -- individual | team
    ADD COLUMN course_type         TEXT NOT NULL DEFAULT 'standard',   -- standard | relay | loop
    ADD COLUMN team_size           INTEGER NULL,       -- entry_type = team only
    -- course_type = loop only
    ADD COLUMN loop_mode           TEXT NULL,          -- target_laps | time_limit
    ADD COLUMN loop_length_km      NUMERIC(6,2) NULL,  -- length of ONE lap
    ADD COLUMN loop_target_laps    INTEGER NULL,       -- loop_mode = target_laps
    ADD COLUMN loop_time_limit_ms  BIGINT NULL;        -- loop_mode = time_limit
```

`loop_length_km` is the length of a single lap. It is not the total race
distance and not a cumulative value. Distances are derived from it:

| Value | Derivation |
|---|---|
| Distance covered by a competitor | `total_laps × loop_length_km` |
| Race distance, `target_laps` | `loop_target_laps × loop_length_km` (may also be stored in `races.distance_km` for display) |
| Race distance, `time_limit` | Not fixed. `races.distance_km` stays `NULL` |

Existing rows default to `individual` / `standard` with all new columns
`NULL`, so the migration is a no-op for existing races.

### 3.2 `teams` (new)

```sql
CREATE TABLE teams (
    id               UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id         UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    race_id          UUID NOT NULL REFERENCES races(id) ON DELETE CASCADE,

    name             TEXT NOT NULL,
    gender_category  TEXT NOT NULL,                       -- male | female | mixed
    status           TEXT NOT NULL DEFAULT 'registered',  -- registered | finisher | dnf | dns

    -- relay: team finish time. loop: time at the last counted lap
    -- (finish time for target_laps, tie-break for time_limit)
    gun_time_ms      BIGINT  NULL,
    net_time_ms      BIGINT  NULL,
    -- loop only: number of counted laps
    total_laps       INTEGER NULL,
    overall_rank     INTEGER NULL,
    -- rank within gender_category
    category_rank    INTEGER NULL,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT teams_race_name_uk UNIQUE (race_id, name)
);
-- + set_updated_at trigger, tenant_id/event_id/race_id indexes, RLS
-- tenant_isolation policy (same shape as participants)
```

### 3.3 `participants`: new columns

```sql
ALTER TABLE participants
    ADD COLUMN team_id    UUID NULL REFERENCES teams(id) ON DELETE CASCADE,
    ADD COLUMN leg_order  INTEGER NULL,  -- relay only: 1-based leg this person runs
    ADD COLUMN gender     TEXT NULL,     -- male | female
    ADD COLUMN total_laps INTEGER NULL;  -- individual loop only

CREATE INDEX participants_team_id_idx ON participants(team_id);
CREATE UNIQUE INDEX participants_team_leg_uk
    ON participants(team_id, leg_order)
    WHERE team_id IS NOT NULL AND leg_order IS NOT NULL;
```

`gender` is needed to validate a team's `gender_category` (§4) and to
rank individual loop races by category.

`last_name` becomes nullable: many runners register with a single name.
The name-search index from `0007_participants` is rebuilt as
`lower(first_name || ' ' || coalesce(last_name, ''))`, since concatenating
a `NULL` would make the indexed expression `NULL` and hide that runner
from name search. Name-search queries must use the same expression.

```sql
ALTER TABLE participants ALTER COLUMN last_name DROP NOT NULL;
DROP INDEX participants_name_lower_idx;
CREATE INDEX participants_name_lower_idx
    ON participants(lower(first_name || ' ' || coalesce(last_name, '')));
```

The existing result columns keep their names, and their meaning depends
on the race format:

| Context | `gun_time_ms` / `net_time_ms` | `overall_rank` / `category_rank` |
|---|---|---|
| Individual standard | Finish time (unchanged) | Set |
| Individual loop | Time at the last counted lap (finish time for `target_laps`, tie-break for `time_limit`) | Set |
| Relay member | **That member's leg time** | `NULL`, ranking lives on `teams` |
| Team loop member | `NULL`. The team's result lives on `teams`, and per-member laps aren't recorded (§2.2) | `NULL` |

## 4. Validation rules (Go, domain layer)

Enforced when a team or participant is created or updated, and when
results are ingested:

**Race configuration**
* `course_type = relay` requires `entry_type = team`.
* `course_type = loop` requires `loop_mode` and `loop_length_km`, plus
  `loop_target_laps` (for `target_laps`) or `loop_time_limit_ms` (for
  `time_limit`).
* `entry_type = team` requires `team_size >= 2`. It is one fixed value,
  not a min/max range: every team in a race has the same number of
  members, so teams compete on equal terms.
* Changing `entry_type` / `course_type` is rejected once the race has
  participants.

**Team membership**
* `participants.team_id` must be set when the race's
  `entry_type = team`, and must be `NULL` when it is `individual`.
* `teams.race_id` must equal the member's `participants.race_id`.
* A team can never have more than `team_size` members (rejected on
  insert). A team with fewer members is allowed while registration is
  still being filled in, and is surfaced as a warning during CSV import
  (a team is complete when it has exactly `team_size` members).

**Relay legs**
* `leg_order` is required for relay members and must be `NULL`
  otherwise.
* `leg_order` values within a team are unique (enforced by
  `participants_team_leg_uk`) and within `1..team_size`.

**Gender category**
* Every member of a team needs `gender` set.
* `male` / `female`: every member has that gender.
* `mixed`: at least one `male` and one `female` member (see §6 open
  question 1).

**Loop results**
* `total_laps` is set only in loop races: on `teams` for team loops, on
  `participants` for individual loops (and `NULL` on team loop members).
* `total_laps >= 0`. In `target_laps` mode, `total_laps <= loop_target_laps`.
* In `time_limit` mode, `net_time_ms <= loop_time_limit_ms`, unless the
  final-lap rule allows otherwise (§6 open question 2).

## 5. Result & ranking rules

| Format | Finisher when | Ordering |
|---|---|---|
| Individual standard | Unchanged | Unchanged |
| Relay | Every member has a leg time | `teams.net_time_ms` asc. Team time = sum of leg times (or the timing system's team finish time, if it provides one) |
| Loop `target_laps` | `total_laps = loop_target_laps` | `net_time_ms` asc. Competitors short of the target are `dnf` |
| Loop `time_limit` | `total_laps >= 1` | `total_laps` desc, then `net_time_ms` (time at the last counted lap) asc |

* A relay team with any member missing a leg time is `dnf`.
* Team `category_rank` is computed within the same `gender_category`.
  Individual loop `category_rank` is computed within the same
  `participants.gender`.

## 6. Open questions

1. **Definition of `mixed`.** Is "at least one of each gender" enough, or
   do some races require a fixed composition (e.g. 2 + 2)? A fixed
   composition would need `races.mixed_min_male` /
   `races.mixed_min_female`.
2. **Final lap in `time_limit` mode.** Does a lap that starts before the
   limit but finishes after it count? Since only totals are stored, the
   rule is applied by whoever submits the result (timing system or
   results import), and Racetify stores the already-counted `total_laps`.
   The rule itself could become a `races` setting if organizers differ.
3. ~~`participants.gender` requirement~~ **Decided:** required for every
   participant in every race format (enforced in Go on create/update and
   import). The column stays `NULL`-able at the DB level for rows that
   predate migration 0012.

## 7. Follow-ons (not in this plan)

These depend on the structure above but are separate plans:

* Participants API: team CRUD, and filtering by team.
* CSV import mapping for team/leg/gender columns (`participants.import`
  job).
* Results ingestion for relay leg times and loop totals (CSV
  `mode=results` and the M2M `timing:write` push).
* E-certificate placeholders for team name, team rank and leg time.
* Runner Portal team result page.
* Dashboard "Data Peserta" per-team view.
* BIB numbering for team races.
