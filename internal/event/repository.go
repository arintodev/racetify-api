package event

import (
	"context"
	"fmt"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// Repository owns every table this bounded context is responsible for -
// events, races, and (assignment_repository.go) event_assignments/
// event_invitations - as one consolidated type, matching the package-per-
// bounded-context pattern's "one repository.go per context" shape (see
// internal/tenant/repository.go's doc comment for the same reasoning).
// Every method is tenant-scoped and must run inside a
// database.DB.WithTenantTx-opened context, or RLS makes it see zero rows -
// except the handful of adminDB-based lookups assignment_repository.go
// documents individually (TenantIDForEvent, MyAssignments), which mirror
// internal/tenant.Repository.ListTenantsForUser's "cross-tenant question,
// answered safely because the WHERE clause is pinned to an
// already-authenticated id" reasoning.
type Repository struct {
	db      *database.DB
	adminDB *database.DB
}

func NewRepository(db, adminDB *database.DB) *Repository {
	return &Repository{db: db, adminDB: adminDB}
}

// ==================== events ====================

const eventColumns = `id, tenant_id, name, slug, venue, start_date, end_date, logo_storage_id, thumbnail_storage_id, status, created_by, created_at, updated_at`

func (r *Repository) CreateEvent(ctx context.Context, e *Event) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO events (id, tenant_id, name, slug, venue, start_date, end_date, logo_storage_id, thumbnail_storage_id, status, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		e.ID, e.TenantID, e.Name, e.Slug, e.Venue, e.StartDate, e.EndDate,
		e.LogoStorageID, e.ThumbnailStorageID, e.Status, e.CreatedBy, e.CreatedAt, e.UpdatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

func (r *Repository) GetEventByID(ctx context.Context, tenantID, id string) (*Event, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return scanEvent(row)
}

// ListEvents returns one keyset-paginated page of tenantID's events,
// newest-first - see internal/platform/pagination's doc comment for why
// keyset rather than LIMIT/OFFSET.
func (r *Repository) ListEvents(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[Event], error) {
	limit := page.NormalizeLimit()
	query := `SELECT ` + eventColumns + ` FROM events WHERE tenant_id = $1`
	args := []any{tenantID}

	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		query += ` AND (created_at, id) < ($2, $3)`
		args = append(args, c.CreatedAt, c.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprint(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[Event]{}, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return pagination.Page[Event]{}, err
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[Event]{}, err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return pagination.Page[Event]{Items: out, NextCursor: next}, nil
}

// UpdateEvent applies a full field update to name/slug/venue/dates/
// storage refs - Service.UpdateEvent is responsible for merging partial
// PATCH input onto the current row before calling this, the same
// "what changed vs. what stays is a service-layer decision" convention
// every other bounded context in this codebase follows. Status is
// deliberately not writable here - see UpdateEventStatus.
func (r *Repository) UpdateEvent(ctx context.Context, e *Event) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE events SET name = $3, slug = $4, venue = $5, start_date = $6, end_date = $7,
			logo_storage_id = $8, thumbnail_storage_id = $9
		WHERE tenant_id = $1 AND id = $2`,
		e.TenantID, e.ID, e.Name, e.Slug, e.Venue, e.StartDate, e.EndDate,
		e.LogoStorageID, e.ThumbnailStorageID,
	)
	if err != nil {
		if dbutil.IsUniqueViolation(err) {
			return domain.ErrAlreadyExists
		}
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// UpdateEventStatus is the sole writer of the draft -> published -> archived
// transition (docs/phase1-api-plan.md §4.1: "publishing is what makes the
// public routes resolve"), kept separate from UpdateEvent so a handler can
// never accidentally change status as a side effect of an unrelated field
// edit.
func (r *Repository) UpdateEventStatus(ctx context.Context, tenantID, id string, status EventStatus) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE events SET status = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, status,
	)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func scanEvent(row dbutil.RowScanner) (*Event, error) {
	e := &Event{}
	err := row.Scan(
		&e.ID, &e.TenantID, &e.Name, &e.Slug, &e.Venue, &e.StartDate, &e.EndDate,
		&e.LogoStorageID, &e.ThumbnailStorageID, &e.Status, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return e, nil
}

// ==================== races ====================

const raceColumns = `id, tenant_id, event_id, name, slug, distance_km, created_at, updated_at`

func (r *Repository) CreateRace(ctx context.Context, race *Race) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO races (id, tenant_id, event_id, name, slug, distance_km, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		race.ID, race.TenantID, race.EventID, race.Name, race.Slug, race.DistanceKM, race.CreatedAt, race.UpdatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

func (r *Repository) GetRaceByID(ctx context.Context, tenantID, eventID, id string) (*Race, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+raceColumns+` FROM races WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		tenantID, eventID, id)
	return scanRace(row)
}

// ListRaces returns every race under an event, oldest-first. Not
// keyset-paginated like ListEvents/ListMembers elsewhere: an event's race
// count is small and bounded (a handful of distances/categories per
// event), unlike participants or photos, so a plain unbounded query
// matches docs/phase1-api-plan.md §4.1's "List races" without needing the
// pagination convention reserved for genuinely large tables.
func (r *Repository) ListRaces(ctx context.Context, tenantID, eventID string) ([]Race, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT `+raceColumns+` FROM races WHERE tenant_id = $1 AND event_id = $2 ORDER BY created_at ASC`,
		tenantID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Race
	for rows.Next() {
		race, err := scanRace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *race)
	}
	return out, rows.Err()
}

func (r *Repository) UpdateRace(ctx context.Context, race *Race) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE races SET name = $4, slug = $5, distance_km = $6
		WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		race.TenantID, race.EventID, race.ID, race.Name, race.Slug, race.DistanceKM,
	)
	if err != nil {
		if dbutil.IsUniqueViolation(err) {
			return domain.ErrAlreadyExists
		}
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// DeleteRace removes a race. "Only if no participants reference it"
// (implementation_guide_phase_1.md §4.1) is enforced by the database
// itself - participants.race_id is ON DELETE RESTRICT
// (migrations/0007_participants.up.sql) - so this method only needs to
// translate that constraint violation into the same domain.ErrInvalidState
// every other "can't do that in this state" error in this codebase uses,
// rather than duplicating the check in Go where it could race a concurrent
// import.
func (r *Repository) DeleteRace(ctx context.Context, tenantID, eventID, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`DELETE FROM races WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		tenantID, eventID, id)
	if err != nil {
		if dbutil.IsForeignKeyViolation(err) {
			return fmt.Errorf("repository: %w: race still has participants", domain.ErrInvalidState)
		}
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func scanRace(row dbutil.RowScanner) (*Race, error) {
	race := &Race{}
	err := row.Scan(&race.ID, &race.TenantID, &race.EventID, &race.Name, &race.Slug, &race.DistanceKM, &race.CreatedAt, &race.UpdatedAt)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return race, nil
}
