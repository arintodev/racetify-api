// Package jobqueue is the generic async-job mechanism shared by every
// Phase 1 background flow - CSV import, BIB batch generation, photo
// thumbnail/watermark/OCR (docs/phase1-api-plan.md §2/§6). It lives at the
// top level, not under internal/platform, because it is app-orchestration
// (it knows about tenant-scoped transactions and audit-adjacent job
// bookkeeping), not raw infrastructure like internal/platform/rediscli or
// internal/platform/database which it is built on top of - the same
// "top-level, not platform" placement internal/audit already uses for the
// same reason (docs/phase1-api-plan.md §9). It imports no bounded-context
// package, so every Phase 1 module (event, participant, generator,
// gallery) can depend on it downward with nothing importing back.
package jobqueue

import (
	"time"

	"github.com/racetify/racetify-api/internal/domain"
)

// Job is one row of the generic async-job table (the `jobs` table,
// migrations/0010_jobs.up.sql). Status/Type reuse internal/domain's
// JobStatus/JobType (see that file's doc comment for why those two enums
// live in internal/domain rather than here: internal/jobqueue,
// cmd/worker, and every module's own job handler all need the same
// vocabulary).
type Job struct {
	ID              string
	TenantID        string
	Type            domain.JobType
	Status          domain.JobStatus
	Payload         map[string]any
	Result          map[string]any
	Error           *string
	ProgressCurrent int
	ProgressTotal   int
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}
