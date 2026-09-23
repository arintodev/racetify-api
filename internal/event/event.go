// Package event is the bounded context for the Wedge strategy's first
// tenant-owned business resource: Events and the Races (categories/
// distances) under them (implementation_guide_phase_1.md §2/§3,
// docs/phase1-api-plan.md §4.1/§9). It is Phase 1's simplest module - no
// async work, no OCR/PDF dependency - built first specifically to prove
// the package-per-bounded-context pattern docs/phase0-refactor-plan.md
// established for Phase 0 extends cleanly to a brand-new Phase 1 resource,
// before anything async (internal/jobqueue, cmd/worker) is layered on top.
package event

import "time"

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
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
