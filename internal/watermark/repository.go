package watermark

import (
	"context"

	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// Repository reads and writes watermarks. Every method runs on the
// connection in ctx, which the service opens as a tenant-scoped transaction
// (WithTenantTx), so row-level security applies on top of the explicit
// tenant_id / event_id conditions used here - the same shape
// gallery.Repository and generator.Repository use.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

const columns = `w.id, w.tenant_id, w.event_id, w.storage_id, w.name, w.anchor_x, w.anchor_y,
	w.offset_x_percent, w.offset_y_percent, w.width_percent, w.aspect_ratio, w.opacity, w.sort_order,
	w.created_at, w.updated_at, o.bucket, o.object_key`

const from = `FROM watermarks w JOIN objects o ON o.id = w.storage_id`

func scan(row dbutil.RowScanner) (*Watermark, error) {
	w := &Watermark{}
	err := row.Scan(&w.ID, &w.TenantID, &w.EventID, &w.StorageID, &w.Name, &w.AnchorX, &w.AnchorY,
		&w.OffsetXPercent, &w.OffsetYPercent, &w.WidthPercent, &w.AspectRatio, &w.Opacity, &w.SortOrder,
		&w.CreatedAt, &w.UpdatedAt, &w.Bucket, &w.ObjectKey)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return w, nil
}

// EventExists proves the event belongs to the tenant (mirrors
// generator.Repository.EventExists).
func (r *Repository) EventExists(ctx context.Context, tenantID, eventID string) error {
	var one int
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT 1 FROM events WHERE id = $1 AND tenant_id = $2`, eventID, tenantID).Scan(&one)
	if err != nil {
		return dbutil.MapNotFound(err)
	}
	return nil
}

// ObjectRef is the part of an objects row checkStorage needs (mirrors
// generator.Repository.GetObject/ObjectRef).
type ObjectRef struct {
	Bucket      string
	ContentType string
	Status      string
}

// GetObject loads the object a watermark would point at.
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

// ListByEvent returns an event's watermarks, in display order.
func (r *Repository) ListByEvent(ctx context.Context, tenantID, eventID string) ([]*Watermark, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT `+columns+` `+from+` WHERE w.tenant_id = $1 AND w.event_id = $2 ORDER BY w.sort_order, w.id`,
		tenantID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Watermark
	for rows.Next() {
		w, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ReplaceAll deletes every watermark of the event and inserts items in its
// place. It does NOT open its own transaction: the caller
// (Service.ReplaceAll) already runs this inside a tenant-scoped transaction
// (database.WithTenantTx), so this just executes SQL against the ctx it is
// given - the same contract every other repository.go method in this
// codebase follows.
func (r *Repository) ReplaceAll(ctx context.Context, tenantID, eventID string, items []*Watermark) error {
	if _, err := r.db.Q(ctx).ExecContext(ctx,
		`DELETE FROM watermarks WHERE tenant_id = $1 AND event_id = $2`, tenantID, eventID); err != nil {
		return err
	}
	for _, w := range items {
		if _, err := r.db.Q(ctx).ExecContext(ctx, `
			INSERT INTO watermarks (id, tenant_id, event_id, storage_id, name, anchor_x, anchor_y,
				offset_x_percent, offset_y_percent, width_percent, aspect_ratio, opacity, sort_order,
				created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)`,
			w.ID, w.TenantID, w.EventID, w.StorageID, w.Name, w.AnchorX, w.AnchorY,
			w.OffsetXPercent, w.OffsetYPercent, w.WidthPercent, w.AspectRatio, w.Opacity, w.SortOrder,
			w.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}
