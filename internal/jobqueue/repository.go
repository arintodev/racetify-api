package jobqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// Repository owns the jobs table. Like internal/tenant.Repository, it
// holds both a plain (RLS-bound) handle and an admin (BYPASSRLS) handle:
// db is used by every tenant-scoped caller (an HTTP handler enqueuing a
// job, or cmd/worker after it has already resolved which tenant a
// dequeued job belongs to and opened that tenant's WithTenantTx);
// adminDB is used solely by GetByIDUnscoped, the one place a genuinely
// cross-tenant lookup is required - see that method's doc comment.
type Repository struct {
	db      *database.DB
	adminDB *database.DB
}

func NewRepository(db, adminDB *database.DB) *Repository {
	return &Repository{db: db, adminDB: adminDB}
}

const jobColumns = `id, tenant_id, type, status, payload, result, error, progress_current, progress_total, created_by, created_at, updated_at, started_at, finished_at`

func (r *Repository) Insert(ctx context.Context, j *Job) error {
	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return err
	}
	_, err = r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO jobs (id, tenant_id, type, status, payload, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		j.ID, j.TenantID, j.Type, j.Status, payload, j.CreatedBy, j.CreatedAt, j.UpdatedAt,
	)
	return err
}

// GetByID looks up a job inside the caller's own tenant context (RLS-
// protected) - used by the GET /api/v1/jobs/{id} polling endpoint
// (docs/phase1-api-plan.md §4.5).
func (r *Repository) GetByID(ctx context.Context, tenantID, id string) (*Job, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return scanJob(row)
}

// GetByIDUnscoped looks up a job by id alone, on the BYPASSRLS admin
// connection, with no tenant context. This is the one place in this
// package that needs it: cmd/worker's poll loop dequeues a bare job id
// from Redis (Queue.Dequeue) with no tenant context to open yet - it must
// learn the job's tenant_id from this row *before* it can open the
// WithTenantTx that every subsequent status-update call
// (MarkProcessing/MarkCompleted/...) requires. This mirrors
// internal/tenant.Repository.ListTenantsForUser's reasoning for the same
// admin-connection pattern: safe here because the lookup key (a job id
// minted server-side, never a user-suppliable identifier reachable outside
// this process) carries no cross-tenant disclosure risk.
func (r *Repository) GetByIDUnscoped(ctx context.Context, id string) (*Job, error) {
	row := r.adminDB.DB.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id)
	return scanJob(row)
}

// List returns one keyset-paginated page of tenantID's jobs, newest-first,
// optionally filtered by type and/or status (docs/phase1-api-plan.md
// §4.5's GET /api/v1/jobs) - see internal/platform/pagination for why
// keyset rather than LIMIT/OFFSET.
func (r *Repository) List(ctx context.Context, tenantID string, page pagination.PageParams, jobType *domain.JobType, status *domain.JobStatus) (pagination.Page[Job], error) {
	limit := page.NormalizeLimit()
	query := `SELECT ` + jobColumns + ` FROM jobs WHERE tenant_id = $1`
	args := []any{tenantID}

	if jobType != nil {
		args = append(args, *jobType)
		query += fmt.Sprintf(" AND type = $%d", len(args))
	}
	if status != nil {
		args = append(args, *status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		args = append(args, c.CreatedAt, c.ID)
		query += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT %d", limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[Job]{}, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return pagination.Page[Job]{}, err
		}
		out = append(out, *j)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[Job]{}, err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return pagination.Page[Job]{Items: out, NextCursor: next}, nil
}

// MarkProcessing transitions a job to 'processing' and stamps started_at -
// the first status update in every job handler's lifecycle
// (docs/phase1-api-plan.md §6).
func (r *Repository) MarkProcessing(ctx context.Context, id string, startedAt time.Time) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE jobs SET status = $2, started_at = $3 WHERE id = $1`,
		id, domain.JobStatusProcessing, startedAt)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// UpdateProgress is called periodically by a long-running job handler
// (e.g. once per photo in a batch) so GET /api/v1/jobs/{id} can report
// meaningful progress before the job finishes.
func (r *Repository) UpdateProgress(ctx context.Context, id string, current, total int) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE jobs SET progress_current = $2, progress_total = $3 WHERE id = $1`,
		id, current, total)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// MarkCompleted transitions a job to 'completed' with its result payload.
func (r *Repository) MarkCompleted(ctx context.Context, id string, result map[string]any, finishedAt time.Time) error {
	return r.finish(ctx, id, domain.JobStatusCompleted, result, nil, finishedAt)
}

// MarkCompletedWithErrors transitions a job to 'completed_with_errors' -
// the batch finished, but some individual items failed (e.g. a handful of
// rows in a CSV import, or a handful of photos in a batch) - result still
// carries whatever succeeded, errMsg summarizes what did not.
func (r *Repository) MarkCompletedWithErrors(ctx context.Context, id string, result map[string]any, errMsg string, finishedAt time.Time) error {
	return r.finish(ctx, id, domain.JobStatusCompletedWithErrors, result, &errMsg, finishedAt)
}

// MarkFailed transitions a job to 'failed'. Per docs/phase1-api-plan.md
// §6, this is terminal - no automatic retry in Phase 1.
func (r *Repository) MarkFailed(ctx context.Context, id string, errMsg string, finishedAt time.Time) error {
	return r.finish(ctx, id, domain.JobStatusFailed, nil, &errMsg, finishedAt)
}

func (r *Repository) finish(ctx context.Context, id string, status domain.JobStatus, result map[string]any, errMsg *string, finishedAt time.Time) error {
	var resultJSON []byte
	if result != nil {
		var err error
		resultJSON, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE jobs SET status = $2, result = $3, error = $4, finished_at = $5 WHERE id = $1`,
		id, status, resultJSON, errMsg, finishedAt)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func scanJob(row dbutil.RowScanner) (*Job, error) {
	j := &Job{}
	var payload, result []byte
	err := row.Scan(
		&j.ID, &j.TenantID, &j.Type, &j.Status, &payload, &result, &j.Error,
		&j.ProgressCurrent, &j.ProgressTotal, &j.CreatedBy, &j.CreatedAt, &j.UpdatedAt,
		&j.StartedAt, &j.FinishedAt,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &j.Payload); err != nil {
			return nil, fmt.Errorf("jobqueue: decode payload: %w", err)
		}
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &j.Result); err != nil {
			return nil, fmt.Errorf("jobqueue: decode result: %w", err)
		}
	}
	return j, nil
}
