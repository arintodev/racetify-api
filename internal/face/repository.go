package face

import (
	"context"
	"database/sql"

	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// Repository reads and writes faces, face_embeddings and
// photo_face_detections. Every method runs on the connection in ctx, which
// the service opens as a tenant-scoped transaction (WithTenantTx), the same
// shape gallery.Repository uses.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

const faceColumns = `id, tenant_id, event_id, user_id, embedding_count, consented_at, consent_revoked_at, created_at, updated_at`

func scanFace(row dbutil.RowScanner) (*Face, error) {
	f := &Face{}
	var revokedAt sql.NullTime
	err := row.Scan(&f.ID, &f.TenantID, &f.EventID, &f.UserID, &f.EmbeddingCount,
		&f.ConsentedAt, &revokedAt, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	if revokedAt.Valid {
		f.ConsentRevokedAt = &revokedAt.Time
	}
	return f, nil
}

// GetFaceByUser fetches a user's enrolled face for an event, if one exists.
func (r *Repository) GetFaceByUser(ctx context.Context, tenantID, eventID, userID string) (*Face, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+faceColumns+` FROM faces WHERE tenant_id = $1 AND event_id = $2 AND user_id = $3`,
		tenantID, eventID, userID)
	return scanFace(row)
}

// GetFace fetches one face of the tenant by id alone.
func (r *Repository) GetFace(ctx context.Context, tenantID, id string) (*Face, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+faceColumns+` FROM faces WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return scanFace(row)
}

// InsertFace creates a new face row. Called only when GetFaceByUser found
// none - the (tenant_id, event_id, user_id) unique index is the ultimate
// guard against a race creating two.
func (r *Repository) InsertFace(ctx context.Context, f *Face) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO faces (id, tenant_id, event_id, user_id, embedding_count, consented_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`,
		f.ID, f.TenantID, f.EventID, f.UserID, f.EmbeddingCount, f.ConsentedAt, f.CreatedAt)
	return err
}

// IncrementEmbeddingCount bumps a face's denormalized embedding_count by 1.
func (r *Repository) IncrementEmbeddingCount(ctx context.Context, tenantID, faceID string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE faces SET embedding_count = embedding_count + 1 WHERE tenant_id = $1 AND id = $2`,
		tenantID, faceID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// RevokeConsent sets consent_revoked_at. Returns domain.ErrNotFound if the
// face does not exist in this tenant, or its consent was already revoked.
func (r *Repository) RevokeConsent(ctx context.Context, tenantID, faceID string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE faces SET consent_revoked_at = now() WHERE tenant_id = $1 AND id = $2 AND consent_revoked_at IS NULL`,
		tenantID, faceID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// ==================== face_embeddings ====================

// InsertEmbedding records one enrollment embedding under a face_id.
func (r *Repository) InsertEmbedding(ctx context.Context, e *Embedding) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO face_embeddings (id, tenant_id, event_id, user_id, face_id, qdrant_point_id, confidence_score, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.ID, e.TenantID, e.EventID, e.UserID, e.FaceID, e.QdrantPointID, e.ConfidenceScore, e.CreatedAt)
	return err
}

// ListEmbeddingsByFace returns every embedding enrolled under a face_id -
// Search uses this to learn which Qdrant point ids represent the face's
// stored vectors.
func (r *Repository) ListEmbeddingsByFace(ctx context.Context, tenantID, faceID string) ([]Embedding, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx, `
		SELECT id, tenant_id, event_id, user_id, face_id, qdrant_point_id, confidence_score, created_at
		FROM face_embeddings WHERE tenant_id = $1 AND face_id = $2 ORDER BY created_at`, tenantID, faceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Embedding
	for rows.Next() {
		var e Embedding
		var conf sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.TenantID, &e.EventID, &e.UserID, &e.FaceID, &e.QdrantPointID, &conf, &e.CreatedAt); err != nil {
			return nil, err
		}
		if conf.Valid {
			e.ConfidenceScore = &conf.Float64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEmbeddingsByFace removes every face_embeddings row of a face_id and
// returns the Qdrant point ids that owned - the caller (Service) must
// delete those points from Qdrant itself, since ON DELETE CASCADE never
// reaches it.
func (r *Repository) DeleteEmbeddingsByFace(ctx context.Context, tenantID, faceID string) ([]string, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`DELETE FROM face_embeddings WHERE tenant_id = $1 AND face_id = $2 RETURNING qdrant_point_id`,
		tenantID, faceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ==================== photo_face_detections ====================

// InsertDetection records one detected face instance. No face_id column:
// see face.go's package doc comment.
func (r *Repository) InsertDetection(ctx context.Context, d *Detection) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO photo_face_detections (id, tenant_id, photo_id, box_x, box_y, box_w, box_h, confidence_score, qdrant_point_id, detected_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		d.ID, d.TenantID, d.PhotoID, d.BoxX, d.BoxY, d.BoxW, d.BoxH, d.ConfidenceScore, d.QdrantPointID, d.DetectedAt)
	return err
}
