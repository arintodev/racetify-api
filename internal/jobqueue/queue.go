package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
	"github.com/racetify/racetify-api/internal/security"
)

// listKey is the single Redis list every job id is pushed to and popped
// from. One shared list (not one per job type) keeps cmd/worker's poll
// loop simple - it dequeues an id, looks up the row to learn its type, and
// hands it to internal/jobqueue.Dispatcher, rather than needing to know
// every type up front to pick which list to block on.
const listKey = "racetify:jobqueue"

// ErrEmpty is returned by Dequeue when no job arrived within blockFor -
// not a failure, just "nothing to do this poll cycle."
var ErrEmpty = errors.New("jobqueue: no job available")

// Queue is the Enqueue/Dequeue mechanism docs/phase1-api-plan.md §6
// describes: a thin wrapper over internal/platform/rediscli (the
// blocking-list transport) and this package's own Repository (the
// durable jobs row every id in the list refers to).
//
// Enqueue and Dequeue insert/read the Redis push in two separate steps
// from the DB insert/read, not inside one atomic operation - a process
// crash between "jobs row inserted" and "id pushed to Redis" leaves an
// orphaned 'queued' row nothing will ever dequeue. This is a known,
// accepted gap: docs/phase1-api-plan.md §6 already defers retry/resilience
// policy to "a reasonable Phase 1.1 follow-up once real failure modes are
// observed", and an outbox-pattern fix for this specific race is the same
// kind of premature-resilience-engineering that section argues against
// building before Phase 1 has a single real job type running.
type Queue struct {
	db    *database.DB
	repo  *Repository
	redis *rediscli.Client
}

func NewQueue(db *database.DB, repo *Repository, redis *rediscli.Client) *Queue {
	return &Queue{db: db, repo: repo, redis: redis}
}

// Enqueue creates a 'queued' jobs row (tenant-scoped, inside its own
// WithTenantTx - the same transactional pattern every other bounded
// context's provisioning call uses) and pushes its id onto the shared
// Redis list for a worker to pick up.
func (q *Queue) Enqueue(ctx context.Context, tenantID, actorUserID string, jobType domain.JobType, payload map[string]any) (string, error) {
	now := time.Now().UTC()
	job := &Job{
		ID:        security.MustNewUUIDv4(),
		TenantID:  tenantID,
		Type:      jobType,
		Status:    domain.JobStatusQueued,
		Payload:   payload,
		CreatedBy: actorUserID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := q.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return q.repo.Insert(ctx, job)
	}); err != nil {
		return "", err
	}

	if err := q.redis.LPush(ctx, listKey, job.ID); err != nil {
		return "", fmt.Errorf("jobqueue: push to redis: %w", err)
	}
	return job.ID, nil
}

// Dequeue blocks up to blockFor waiting for a job id to arrive, then
// resolves it to a full Job row via Repository.GetByIDUnscoped (see that
// method's doc comment for why an unscoped lookup is what a bare id from
// Redis requires). Returns ErrEmpty, not an error, when nothing arrived
// within blockFor - the normal "idle poll cycle" outcome cmd/worker's loop
// expects and simply repeats on.
func (q *Queue) Dequeue(ctx context.Context, blockFor time.Duration) (*Job, error) {
	id, err := q.redis.BRPop(ctx, listKey, blockFor)
	if err != nil {
		if errors.Is(err, rediscli.ErrNil) {
			return nil, ErrEmpty
		}
		return nil, err
	}
	return q.repo.GetByIDUnscoped(ctx, id)
}

// ---- status-update convenience methods, used by Run's poll loop ----
//
// Each opens its own short WithTenantTx (job.TenantID is already known by
// this point - GetByIDUnscoped resolved it) rather than the caller having
// to reach into Repository directly, keeping cmd/worker's loop (runner.go)
// talking only to Queue/Dispatcher, the same "callers use the Service, not
// the Repository" shape every HTTP-facing bounded context in this codebase
// already follows.

func (q *Queue) MarkProcessing(ctx context.Context, job *Job) error {
	return q.db.WithTenantTx(ctx, job.TenantID, func(ctx context.Context) error {
		return q.repo.MarkProcessing(ctx, job.ID, time.Now().UTC())
	})
}

func (q *Queue) MarkCompleted(ctx context.Context, job *Job, result map[string]any) error {
	return q.db.WithTenantTx(ctx, job.TenantID, func(ctx context.Context) error {
		return q.repo.MarkCompleted(ctx, job.ID, result, time.Now().UTC())
	})
}

func (q *Queue) MarkCompletedWithErrors(ctx context.Context, job *Job, result map[string]any, errMsg string) error {
	return q.db.WithTenantTx(ctx, job.TenantID, func(ctx context.Context) error {
		return q.repo.MarkCompletedWithErrors(ctx, job.ID, result, errMsg, time.Now().UTC())
	})
}

func (q *Queue) MarkFailed(ctx context.Context, job *Job, errMsg string) error {
	return q.db.WithTenantTx(ctx, job.TenantID, func(ctx context.Context) error {
		return q.repo.MarkFailed(ctx, job.ID, errMsg, time.Now().UTC())
	})
}

// UpdateProgress lets a long-running handler (e.g. once per photo in a
// batch) report incremental progress mid-job.
func (q *Queue) UpdateProgress(ctx context.Context, job *Job, current, total int) error {
	return q.db.WithTenantTx(ctx, job.TenantID, func(ctx context.Context) error {
		return q.repo.UpdateProgress(ctx, job.ID, current, total)
	})
}

// GetJob looks up a job inside the caller's own tenant context - the
// GET /api/v1/jobs/{id} polling endpoint's read path (handler.go).
func (q *Queue) GetJob(ctx context.Context, tenantID, id string) (*Job, error) {
	var job *Job
	err := q.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		job, err = q.repo.GetByID(ctx, tenantID, id)
		return err
	})
	return job, err
}

// ListJobs returns one keyset-paginated page of tenantID's jobs.
func (q *Queue) ListJobs(ctx context.Context, tenantID string, page pagination.PageParams, jobType *domain.JobType, status *domain.JobStatus) (pagination.Page[Job], error) {
	var out pagination.Page[Job]
	err := q.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = q.repo.List(ctx, tenantID, page, jobType, status)
		return err
	})
	return out, err
}
