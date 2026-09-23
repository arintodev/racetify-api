package domain

// JobStatus is the lifecycle of an async background job (the `jobs` table,
// migrations/0010_jobs.up.sql). It lives in internal/domain - not inside
// internal/jobqueue or any one bounded context - because it is a genuinely
// shared platform concept with no invariant any single module owns:
// internal/jobqueue's Enqueue/Dequeue, cmd/worker's poll loop, and every
// Phase 1 module's own job handler (participant/import, generator,
// gallery/jobs) all read and write the same enum, matching how
// ObjectStatus (Phase 0) already lives here for the same reason.
type JobStatus string

const (
	JobStatusQueued              JobStatus = "queued"
	JobStatusProcessing          JobStatus = "processing"
	JobStatusCompleted           JobStatus = "completed"
	JobStatusCompletedWithErrors JobStatus = "completed_with_errors"
	JobStatusFailed              JobStatus = "failed"
)

// JobType names which handler a queued job dispatches to. Each Phase 1
// bounded context that does async work registers exactly one of these with
// internal/jobqueue's Dispatcher (docs/phase1-api-plan.md §6/§9).
type JobType string

const (
	JobTypeParticipantsImport JobType = "participants.import"
	JobTypeGeneratorBibBatch  JobType = "generator.bib_batch"
	JobTypeMediaPhotoProcess  JobType = "media.photo_process"
)
