package face

import (
	"context"
	"database/sql"

	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// Repository reads and writes faces, face_embeddings and
// photo_face_detections. Every method runs on the connection in ctx, which
// the service opens as either db.WithTx (self-enroll, no tenant) or
// db.WithTenantTx (tenant M2M) - see docs/face-tenant-enrollment-plan.md
// §2.3 for why faces/face_embeddings' RLS policy makes both shapes safe.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

const faceColumns = `id, tenant_id, user_id, ref_id, embedding_count, consented_at, created_at, updated_at`

func scanFace(row dbutil.RowScanner) (*Face, error) {
	f := &Face{}
	var tenantID, userID, refID sql.NullString
	err := row.Scan(&f.ID, &tenantID, &userID, &refID, &f.EmbeddingCount,
		&f.ConsentedAt, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	if tenantID.Valid {
		f.TenantID = &tenantID.String
	}
	if userID.Valid {
		f.UserID = &userID.String
	}
	if refID.Valid {
		f.RefID = &refID.String
	}
	return f, nil
}

// GetFaceByUser fetches a Racetify account's own global face, if one
// exists. Relies on the caller running inside db.WithTx (no tenant
// context) so the RLS policy's tenant_id IS NULL branch is what actually
// scopes this to global rows only.
func (r *Repository) GetFaceByUser(ctx context.Context, userID string) (*Face, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+faceColumns+` FROM faces WHERE tenant_id IS NULL AND user_id = $1`, userID)
	return scanFace(row)
}

// GetFaceByRef fetches a tenant's ref-based face, if one exists.
func (r *Repository) GetFaceByRef(ctx context.Context, tenantID, refID string) (*Face, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+faceColumns+` FROM faces WHERE tenant_id = $1 AND ref_id = $2`, tenantID, refID)
	return scanFace(row)
}

// GetFace fetches one face by id alone - no ownership/tenant filter of
// its own. Callers that need to restrict who a face_id resolves to (Drop)
// must apply that check themselves against the returned Face; Search
// intentionally does not, relying only on whatever the active
// transaction's RLS scope already let through (docs/face-tenant-
// enrollment-plan.md §3.3).
func (r *Repository) GetFace(ctx context.Context, id string) (*Face, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx, `SELECT `+faceColumns+` FROM faces WHERE id = $1`, id)
	return scanFace(row)
}

// InsertFace creates a new face row. Called only when GetFaceByUser/
// GetFaceByRef found none - the relevant partial unique index
// (faces_user_uk or faces_tenant_ref_uk) is the ultimate guard against a
// race creating two.
func (r *Repository) InsertFace(ctx context.Context, f *Face) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO faces (id, tenant_id, user_id, ref_id, embedding_count, consented_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`,
		f.ID, f.TenantID, f.UserID, f.RefID, f.EmbeddingCount, f.ConsentedAt, f.CreatedAt)
	return err
}

// IncrementEmbeddingCount bumps a face's denormalized embedding_count by 1.
func (r *Repository) IncrementEmbeddingCount(ctx context.Context, faceID string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE faces SET embedding_count = embedding_count + 1 WHERE id = $1`, faceID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// DropFace hard-deletes faceID: every face_embeddings row under it (the
// FK's ON DELETE CASCADE would do this on its own, but the query form
// here is needed anyway to recover their qdrant_point_ids) and the faces
// row itself. The caller (Service.dropFace) must delete those points from
// Qdrant, since ON DELETE CASCADE never reaches it, and must have already
// verified the caller is allowed to drop this particular face - this
// method itself applies no ownership filter beyond "this id exists".
func (r *Repository) DropFace(ctx context.Context, faceID string) ([]string, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`DELETE FROM face_embeddings WHERE face_id = $1 RETURNING qdrant_point_id`, faceID)
	if err != nil {
		return nil, err
	}
	var pointIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		pointIDs = append(pointIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	res, err := r.db.Q(ctx).ExecContext(ctx, `DELETE FROM faces WHERE id = $1`, faceID)
	if err != nil {
		return nil, err
	}
	if err := dbutil.CheckRowsAffected(res); err != nil {
		return nil, err
	}
	return pointIDs, nil
}

// ==================== face_embeddings ====================

// InsertEmbedding records one enrollment embedding under a face_id.
func (r *Repository) InsertEmbedding(ctx context.Context, e *Embedding) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO face_embeddings (id, tenant_id, user_id, ref_id, face_id, qdrant_point_id, confidence_score, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.ID, e.TenantID, e.UserID, e.RefID, e.FaceID, e.QdrantPointID, e.ConfidenceScore, e.CreatedAt)
	return err
}

// ListEmbeddingsByFace returns every embedding enrolled under a face_id -
// Search uses this to learn which Qdrant point ids represent the face's
// stored vectors. No tenant/subject filter beyond face_id: by the time
// this is called, the caller has already resolved (and, where needed,
// authorized) the parent Face.
func (r *Repository) ListEmbeddingsByFace(ctx context.Context, faceID string) ([]Embedding, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx, `
		SELECT id, tenant_id, user_id, ref_id, face_id, qdrant_point_id, confidence_score, created_at
		FROM face_embeddings WHERE face_id = $1 ORDER BY created_at`, faceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Embedding
	for rows.Next() {
		var e Embedding
		var tenantID, userID, refID sql.NullString
		var conf sql.NullFloat64
		if err := rows.Scan(&e.ID, &tenantID, &userID, &refID, &e.FaceID, &e.QdrantPointID, &conf, &e.CreatedAt); err != nil {
			return nil, err
		}
		if tenantID.Valid {
			e.TenantID = &tenantID.String
		}
		if userID.Valid {
			e.UserID = &userID.String
		}
		if refID.Valid {
			e.RefID = &refID.String
		}
		if conf.Valid {
			e.ConfidenceScore = &conf.Float64
		}
		out = append(out, e)
	}
	return out, rows.Err()
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
