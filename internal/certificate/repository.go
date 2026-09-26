package certificate

import (
	"context"
	"errors"
	"time"

	"github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// Repository reads and writes certificates. Every method runs on the
// connection in ctx, which the service opens as a tenant-scoped transaction
// (WithTenantTx), so row-level security applies on top of the explicit
// tenant_id / event_id conditions used here.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

const columns = `c.id, c.tenant_id, c.event_id, c.participant_id, c.certificate_no, c.template_id,
	c.template_version, c.data_hash, c.status, c.error, c.storage_id, c.format, c.dpi, c.width_px,
	c.height_px, c.size_bytes, c.generated_at, c.created_by, c.created_at, c.updated_at,
	COALESCE(o.bucket, ''), COALESCE(o.object_key, '')`

const from = `FROM certificates c LEFT JOIN objects o ON o.id = c.storage_id`

func scan(row dbutil.RowScanner) (*Certificate, error) {
	c := &Certificate{}
	err := row.Scan(
		&c.ID, &c.TenantID, &c.EventID, &c.ParticipantID, &c.CertificateNo, &c.TemplateID,
		&c.TemplateVersion, &c.DataHash, &c.Status, &c.Error, &c.StorageID, &c.Format, &c.DPI, &c.WidthPx,
		&c.HeightPx, &c.SizeBytes, &c.GeneratedAt, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		&c.Bucket, &c.ObjectKey,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return c, nil
}

func numberTaken(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == "certificates_no_uk"
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

// ParticipantInEvent proves the participant belongs to the event.
func (r *Repository) ParticipantInEvent(ctx context.Context, tenantID, eventID, participantID string) error {
	var one int
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT 1 FROM participants WHERE id = $1 AND tenant_id = $2 AND event_id = $3`,
		participantID, tenantID, eventID).Scan(&one)
	if err != nil {
		return dbutil.MapNotFound(err)
	}
	return nil
}

// TemplateInEvent reports whether templateID is a certificate template of the event.
func (r *Repository) TemplateInEvent(ctx context.Context, tenantID, eventID, templateID string) (bool, error) {
	var one int
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT 1 FROM generator_templates WHERE id = $1 AND tenant_id = $2 AND event_id = $3 AND service = 'certificate'`,
		templateID, tenantID, eventID).Scan(&one)
	if err != nil {
		if errors.Is(dbutil.MapNotFound(err), domain.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ObjectRef is the part of an objects row a certificate needs to check.
type ObjectRef struct {
	Bucket      string
	ContentType string
	Status      string
}

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

// List returns every certificate of the event, by participant.
func (r *Repository) List(ctx context.Context, tenantID, eventID string) ([]Certificate, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT `+columns+` `+from+` WHERE c.tenant_id = $1 AND c.event_id = $2 ORDER BY c.participant_id`,
		tenantID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Certificate{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (r *Repository) GetByParticipant(ctx context.Context, tenantID, eventID, participantID string) (*Certificate, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+columns+` `+from+` WHERE c.tenant_id = $1 AND c.event_id = $2 AND c.participant_id = $3`,
		tenantID, eventID, participantID)
	return scan(row)
}

// Upsert writes the record of a participant. The certificate number is kept
// when the record exists; on a failed run the details of the earlier file are
// kept too (it stays downloadable), so the nullable columns only overwrite
// when given.
func (r *Repository) Upsert(ctx context.Context, c *Certificate) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO certificates (id, tenant_id, event_id, participant_id, certificate_no, template_id,
			template_version, data_hash, status, error, storage_id, format, dpi, width_px, height_px,
			size_bytes, generated_at, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $19)
		ON CONFLICT (participant_id) DO UPDATE SET
			template_id = EXCLUDED.template_id,
			template_version = EXCLUDED.template_version,
			data_hash = EXCLUDED.data_hash,
			status = EXCLUDED.status,
			error = EXCLUDED.error,
			storage_id = COALESCE(EXCLUDED.storage_id, certificates.storage_id),
			format = COALESCE(EXCLUDED.format, certificates.format),
			dpi = COALESCE(EXCLUDED.dpi, certificates.dpi),
			width_px = COALESCE(EXCLUDED.width_px, certificates.width_px),
			height_px = COALESCE(EXCLUDED.height_px, certificates.height_px),
			size_bytes = COALESCE(EXCLUDED.size_bytes, certificates.size_bytes),
			generated_at = COALESCE(EXCLUDED.generated_at, certificates.generated_at)`,
		c.ID, c.TenantID, c.EventID, c.ParticipantID, c.CertificateNo, c.TemplateID,
		c.TemplateVersion, c.DataHash, c.Status, c.Error, c.StorageID, c.Format, c.DPI, c.WidthPx, c.HeightPx,
		c.SizeBytes, c.GeneratedAt, c.CreatedBy, c.CreatedAt)
	if numberTaken(err) {
		return conflict("certificate_no_taken", "certificate_no", "This certificate number is already used.")
	}
	return err
}

func (r *Repository) Delete(ctx context.Context, tenantID, eventID, participantID string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`DELETE FROM certificates WHERE tenant_id = $1 AND event_id = $2 AND participant_id = $3`,
		tenantID, eventID, participantID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// PublishedAt is when the event's certificates were published; nil when not.
func (r *Repository) PublishedAt(ctx context.Context, tenantID, eventID string) (*time.Time, error) {
	var at time.Time
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT published_at FROM certificate_publications WHERE tenant_id = $1 AND event_id = $2`,
		tenantID, eventID).Scan(&at)
	if err != nil {
		if errors.Is(dbutil.MapNotFound(err), domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &at, nil
}

func (r *Repository) SetPublished(ctx context.Context, tenantID, eventID string, at *time.Time) error {
	if at == nil {
		_, err := r.db.Q(ctx).ExecContext(ctx,
			`DELETE FROM certificate_publications WHERE tenant_id = $1 AND event_id = $2`, tenantID, eventID)
		return err
	}
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO certificate_publications (event_id, tenant_id, published_at) VALUES ($1, $2, $3)
		ON CONFLICT (event_id) DO UPDATE SET published_at = EXCLUDED.published_at`,
		eventID, tenantID, *at)
	return err
}
