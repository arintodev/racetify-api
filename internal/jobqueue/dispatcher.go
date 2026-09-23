package jobqueue

import (
	"context"
	"errors"
	"fmt"

	"github.com/racetify/racetify-api/internal/domain"
)

// ErrNoHandler is returned by Dispatch when no handler was registered for
// a job's type - e.g. a rolling deploy where the API process (which can
// enqueue a new job type) is newer than the worker process currently
// running.
var ErrNoHandler = errors.New("jobqueue: no handler registered for this job type")

// HandlerFunc processes one dequeued Job. Three outcomes, matching
// internal/domain.JobStatus's three terminal states:
//   - err != nil: the job failed outright -> Run calls Queue.MarkFailed.
//   - err == nil && partialErr != "": the batch finished but some items
//     within it failed (e.g. a few rows of a CSV import) -> Run calls
//     Queue.MarkCompletedWithErrors.
//   - err == nil && partialErr == "": full success -> Run calls
//     Queue.MarkCompleted.
//
// A HandlerFunc is responsible for its own tenant-scoped database access
// (via database.DB.WithTenantTx(job.TenantID, ...)) rather than being
// handed an already-open transaction - see Run's doc comment for why.
type HandlerFunc func(ctx context.Context, job *Job) (result map[string]any, partialErr string, err error)

// Dispatcher maps a domain.JobType to the HandlerFunc that processes it.
// Each Phase 1 module that does async work registers exactly one entry
// here from cmd/worker/main.go (docs/phase1-api-plan.md §6/§9:
// participant/import's job handler, generator's bulk-BIB handler,
// gallery/jobs' thumbnail+OCR handlers). This package itself never imports
// a bounded-context package - only cmd/worker imports both sides to wire
// them together, the same "wiring, not construction" role router.go plays
// for HTTP routes (docs/phase1-api-plan.md §9's Problem 2).
type Dispatcher struct {
	handlers map[domain.JobType]HandlerFunc
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{handlers: make(map[domain.JobType]HandlerFunc)}
}

// Register adds jobType's handler. Registering the same type twice is a
// startup wiring bug (two modules claiming the same job type), so it
// panics immediately rather than letting the second registration silently
// win - cmd/worker's wiring happens once, at boot, before the poll loop
// starts, so failing loudly there is safe and far preferable to a subtle
// runtime routing bug.
func (d *Dispatcher) Register(jobType domain.JobType, h HandlerFunc) {
	if _, exists := d.handlers[jobType]; exists {
		panic(fmt.Sprintf("jobqueue: handler already registered for job type %q", jobType))
	}
	d.handlers[jobType] = h
}

// Dispatch looks up and invokes job.Type's handler.
func (d *Dispatcher) Dispatch(ctx context.Context, job *Job) (result map[string]any, partialErr string, err error) {
	h, ok := d.handlers[job.Type]
	if !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrNoHandler, job.Type)
	}
	return h(ctx, job)
}
