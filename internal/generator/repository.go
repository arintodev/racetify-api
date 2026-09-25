package generator

import (
	"context"
	"errors"

	"github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// Repository reads and writes generator_templates. Every method runs on the
// connection in ctx, which the service opens as a tenant-scoped transaction
// (WithTenantTx), so row-level security applies on top of the explicit
// tenant_id / event_id conditions used here.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

const templateColumns = `t.id, t.tenant_id, t.event_id, t.race_id, t.storage_id, t.name, t.service,
	t.is_active, t.version, t.metadata, t.created_by, t.created_at, t.updated_at,
	o.bucket, o.object_key, o.content_type, o.status`

const templateFrom = `FROM generator_templates t JOIN objects o ON o.id = t.storage_id`

func scanTemplate(row dbutil.RowScanner) (*Template, error) {
	t := &Template{}
	var kind string
	var metadata []byte
	err := row.Scan(
		&t.ID, &t.TenantID, &t.EventID, &t.RaceID, &t.StorageID, &t.Name, &kind,
		&t.IsActive, &t.Version, &metadata, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt,
		&t.Bucket, &t.ObjectKey, &t.ContentType, &t.ObjectState,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	t.Kind = Kind(kind)
	t.Metadata = metadata
	return t, nil
}

// scopeTaken reports a unique violation of the one-active-template-per-scope
// index (two writers racing for the same scope).
func scopeTaken(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == "generator_templates_scope_uk"
}

// EventExists proves the event belongs to the tenant.
func (r *Repository) EventExists(ctx context.Context, tenantID, eventID string) error {
	var one int
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT 1 FROM events WHERE id = $1 AND tenant_id = $2`, eventID, tenantID).Scan(&one)
	if err != nil {
		return dbutil.MapNotFound(err)
	}
	return nil
}

// RaceInEvent reports whether raceID is a race of the event.
func (r *Repository) RaceInEvent(ctx context.Context, tenantID, eventID, raceID string) (bool, error) {
	var one int
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT 1 FROM races WHERE id = $1 AND tenant_id = $2 AND event_id = $3`, raceID, tenantID, eventID).Scan(&one)
	if err != nil {
		if errors.Is(dbutil.MapNotFound(err), domain.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ObjectRef is the part of an objects row a template needs to check.
type ObjectRef struct {
	Bucket      string
	ContentType string
	Status      string
}

// GetObject loads the object a template would point at.
func (r *Repository) GetObject(ctx context.Context, tenantID, storageID string) (*ObjectRef, error) {
	o := &ObjectRef{}
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT bucket, content_type, status FROM objects WHERE id = $1 AND tenant_id = $2`,
		storageID, tenantID).Scan(&o.Bucket, &o.ContentType, &o.Status)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return o, nil
}

// List returns an event's templates, newest edit first; kind "" means all.
func (r *Repository) List(ctx context.Context, tenantID, eventID string, kind Kind) ([]Template, error) {
	query := `SELECT ` + templateColumns + ` ` + templateFrom + ` WHERE t.tenant_id = $1 AND t.event_id = $2`
	args := []any{tenantID, eventID}
	if kind != "" {
		query += ` AND t.service = $3`
		args = append(args, string(kind))
	}
	query += ` ORDER BY t.updated_at DESC, t.id`
	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (r *Repository) Get(ctx context.Context, tenantID, eventID, id string) (*Template, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+templateColumns+` `+templateFrom+` WHERE t.tenant_id = $1 AND t.event_id = $2 AND t.id = $3`,
		tenantID, eventID, id)
	return scanTemplate(row)
}

func (r *Repository) Create(ctx context.Context, t *Template) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO generator_templates (id, tenant_id, event_id, race_id, storage_id, name, service,
			is_active, version, metadata, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $12)`,
		t.ID, t.TenantID, t.EventID, t.RaceID, t.StorageID, t.Name, string(t.Kind),
		t.IsActive, t.Version, string(t.Metadata), t.CreatedBy, t.CreatedAt)
	if scopeTaken(err) {
		return conflict("scope_taken", "race_id", "Another template already holds this scope.")
	}
	return err
}

// Update writes the editable fields of t (never the kind).
func (r *Repository) Update(ctx context.Context, t *Template) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE generator_templates
		SET race_id = $4, storage_id = $5, name = $6, is_active = $7, version = $8, metadata = $9::jsonb
		WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		t.TenantID, t.EventID, t.ID, t.RaceID, t.StorageID, t.Name, t.IsActive, t.Version, string(t.Metadata))
	if scopeTaken(err) {
		return conflict("scope_taken", "race_id", "Another template already holds this scope.")
	}
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// Deactivate turns off whichever active template of `kind` holds the scope
// (raceID, nil for the event default), other than exceptID. It returns how
// many it turned off.
func (r *Repository) Deactivate(ctx context.Context, tenantID, eventID string, kind Kind, raceID *string, exceptID string) (int64, error) {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE generator_templates SET is_active = false
		WHERE tenant_id = $1 AND event_id = $2 AND service = $3 AND is_active
		  AND race_id IS NOT DISTINCT FROM $4::uuid AND id <> $5`,
		tenantID, eventID, string(kind), raceID, exceptID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *Repository) Delete(ctx context.Context, tenantID, eventID, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`DELETE FROM generator_templates WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		tenantID, eventID, id)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}
