// Package event is the bounded context for the Wedge strategy's first
// tenant-owned business resource: Events and the Races (categories/
// distances) under them (implementation_guide_phase_1.md §2/§3,
// docs/phase1-api-plan.md §4.1/§9). It is Phase 1's simplest module - no
// async work, no OCR/PDF dependency - built first specifically to prove
// the package-per-bounded-context pattern docs/phase0-refactor-plan.md
// established for Phase 0 extends cleanly to a brand-new Phase 1 resource,
// before anything async (internal/jobqueue, cmd/worker) is layered on top.
package event

import (
	"fmt"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
)

// EventStatus is a plain string, not a DB-level enum/CHECK constraint -
// see migrations/0001_core.up.sql's doc comment for why the legal set is
// enforced once, in Go.
type EventStatus string

const (
	EventStatusDraft     EventStatus = "draft"
	EventStatusPublished EventStatus = "published"
	EventStatusArchived  EventStatus = "archived"
)

// Event is an Organizer's race event - the resource every other Phase 1
// module (Race, Participant, GeneratorTemplate, Album) hangs off. Only a
// 'published' Event ever resolves through the unauthenticated Runner
// Portal (internal/portal, once built - docs/phase1-api-plan.md §5).
type Event struct {
	ID       string
	TenantID string
	Name     string
	Slug     string
	Venue    *string
	// StartDate/EndDate are calendar dates (no time-of-day, no timezone) -
	// migrations/0006_events_races.up.sql stores them as Postgres DATE.
	StartDate *time.Time
	EndDate   *time.Time
	// Both nullable FKs into the existing Phase 0 objects table - see
	// migrations/0006_events_races.up.sql's doc comment for why an FK
	// (not inline bucket/key columns) is what keeps this portable across
	// storage providers.
	LogoStorageID      *string
	ThumbnailStorageID *string
	Status             EventStatus
	CreatedBy          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Race is a category/distance under an Event (e.g. "10K", "Half
// Marathon"). It has no invariant of its own beyond belonging to exactly
// one Event - the Organizer manages both together - so Race lives in this
// package rather than getting its own bounded context
// (docs/phase1-api-plan.md §9).
type Race struct {
	ID         string
	TenantID   string
	EventID    string
	Name       string
	Slug       string
	DistanceKM *float64
	RaceFormat
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RaceEntryType is who competes in a race: a person or a team
// (docs/team-loop-participants-plan.md §2.1).
type RaceEntryType string

const (
	RaceEntryIndividual RaceEntryType = "individual"
	RaceEntryTeam       RaceEntryType = "team"
)

// RaceCourseType is how a race's distance is run
// (docs/team-loop-participants-plan.md §2.1).
type RaceCourseType string

const (
	RaceCourseStandard RaceCourseType = "standard"
	RaceCourseRelay    RaceCourseType = "relay"
	RaceCourseLoop     RaceCourseType = "loop"
)

// LoopMode decides how a loop race is won: fastest to complete
// LoopTargetLaps, or most laps within LoopTimeLimitMS.
type LoopMode string

const (
	LoopModeTargetLaps LoopMode = "target_laps"
	LoopModeTimeLimit  LoopMode = "time_limit"
)

// RaceFormat is a race's entry/course configuration
// (migrations/0012_team_loop_formats.up.sql). The zero value, after
// Normalize, is today's individual/standard race.
type RaceFormat struct {
	EntryType  RaceEntryType
	CourseType RaceCourseType
	// TeamSize is the fixed number of members every team in the race
	// has - one value, not a range, so teams compete on equal terms.
	TeamSize *int
	LoopMode *LoopMode
	// LoopLengthKM is the length of ONE lap - not the total race
	// distance, not cumulative.
	LoopLengthKM    *float64
	LoopTargetLaps  *int
	LoopTimeLimitMS *int64
}

// Normalize fills in the default axes and clears every field that
// doesn't apply to the chosen format - so switching a race from loop back
// to standard via PATCH (where nil means "unchanged") doesn't leave stale
// loop settings behind.
func (f *RaceFormat) Normalize() {
	if f.EntryType == "" {
		f.EntryType = RaceEntryIndividual
	}
	if f.CourseType == "" {
		f.CourseType = RaceCourseStandard
	}
	if f.EntryType != RaceEntryTeam {
		f.TeamSize = nil
	}
	if f.CourseType != RaceCourseLoop {
		f.LoopMode, f.LoopLengthKM, f.LoopTargetLaps, f.LoopTimeLimitMS = nil, nil, nil, nil
		return
	}
	if f.LoopMode != nil {
		switch *f.LoopMode {
		case LoopModeTargetLaps:
			f.LoopTimeLimitMS = nil
		case LoopModeTimeLimit:
			f.LoopTargetLaps = nil
		}
	}
}

// Validate enforces docs/team-loop-participants-plan.md §4's race
// configuration rules. Call Normalize first.
func (f *RaceFormat) Validate() error {
	switch f.EntryType {
	case RaceEntryIndividual, RaceEntryTeam:
	default:
		return invalidFormat("entry_type must be individual or team")
	}
	switch f.CourseType {
	case RaceCourseStandard, RaceCourseRelay, RaceCourseLoop:
	default:
		return invalidFormat("course_type must be standard, relay or loop")
	}
	if f.CourseType == RaceCourseRelay && f.EntryType != RaceEntryTeam {
		return invalidFormat("a relay race must have entry_type team")
	}
	if f.EntryType == RaceEntryTeam && f.CourseType == RaceCourseStandard {
		return invalidFormat("a team race must have course_type relay or loop")
	}

	if f.EntryType == RaceEntryTeam {
		if f.TeamSize == nil {
			return invalidFormat("team_size is required for a team race")
		}
		if *f.TeamSize < 2 {
			return invalidFormat("team_size must be at least 2")
		}
	}

	if f.CourseType == RaceCourseLoop {
		if f.LoopMode == nil {
			return invalidFormat("loop_mode is required for a loop race")
		}
		if f.LoopLengthKM == nil || *f.LoopLengthKM <= 0 {
			return invalidFormat("loop_length_km must be greater than 0 for a loop race")
		}
		switch *f.LoopMode {
		case LoopModeTargetLaps:
			if f.LoopTargetLaps == nil || *f.LoopTargetLaps < 1 {
				return invalidFormat("loop_target_laps must be at least 1 for loop_mode target_laps")
			}
		case LoopModeTimeLimit:
			if f.LoopTimeLimitMS == nil || *f.LoopTimeLimitMS <= 0 {
				return invalidFormat("loop_time_limit_ms must be greater than 0 for loop_mode time_limit")
			}
		default:
			return invalidFormat("loop_mode must be target_laps or time_limit")
		}
	}
	return nil
}

// Equal reports whether two formats are identical - used to reject a
// format change on a race that already has participants.
func (f RaceFormat) Equal(o RaceFormat) bool {
	return f.EntryType == o.EntryType &&
		f.CourseType == o.CourseType &&
		eqPtr(f.TeamSize, o.TeamSize) &&
		eqPtr(f.LoopMode, o.LoopMode) &&
		eqPtr(f.LoopLengthKM, o.LoopLengthKM) &&
		eqPtr(f.LoopTargetLaps, o.LoopTargetLaps) &&
		eqPtr(f.LoopTimeLimitMS, o.LoopTimeLimitMS)
}

func eqPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func invalidFormat(msg string) error {
	return fmt.Errorf("service: %w: %s", domain.ErrInvalidState, msg)
}
