package gallery

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// DuplicateKeys returns which of the given files (name + size of the file
// the photographer picked) already exist as photos in the album, as
// "name|size" keys.
func (r *Repository) DuplicateKeys(ctx context.Context, tenantID, albumID string, names []string, sizes []int64) (map[string]bool, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx, `
		SELECT p.original_filename, p.original_size
		FROM photos p
		JOIN unnest($3::text[], $4::bigint[]) AS f(name, size)
		  ON p.original_filename = f.name AND p.original_size = f.size
		WHERE p.tenant_id = $1 AND p.album_id = $2`,
		tenantID, albumID, pq.Array(names), pq.Array(sizes))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			return nil, err
		}
		out[DuplicateKey(name, size)] = true
	}
	return out, rows.Err()
}

// DuplicateKey is how two picks of the same file are told apart.
func DuplicateKey(name string, size int64) string { return fmt.Sprintf("%s|%d", name, size) }

// InsertPhoto adds a photo. It reports false, without an error, when the
// upload was already completed (same stored object, or same file already in
// the album), so completing twice is harmless.
func (r *Repository) InsertPhoto(ctx context.Context, p *Photo, storageID string) (bool, error) {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO photos (id, tenant_id, album_id, original_storage_id, ocr_status, created_by,
			original_filename, original_size, size, width, height, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9,$10,$11,$11)
		ON CONFLICT DO NOTHING`,
		p.ID, p.TenantID, p.AlbumID, storageID, p.CreatedBy,
		p.OriginalFilename, p.OriginalSize, p.Size, p.Width, p.Height, p.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PhotoIDByStorage finds the photo already made from an uploaded object.
func (r *Repository) PhotoIDByStorage(ctx context.Context, tenantID, storageID string) (string, error) {
	var id string
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT id FROM photos WHERE tenant_id = $1 AND original_storage_id = $2`, tenantID, storageID).Scan(&id)
	return id, dbutil.MapNotFound(err)
}

// PhotoFilter narrows ListPhotos. Empty fields do not filter.
type PhotoFilter struct {
	AlbumID string
	// State is one of the derived states (§3.3).
	State State
	// BIB matches a tag exactly as typed (§5: no normalisation).
	BIB string
	// UploaderID is the photographer.
	UploaderID string
}

// stateCondition mirrors §3.3's rule order against the aggregate columns of
// the tags lateral join (t.n tags, t.review OCR tags under the auto
// threshold, t.manual manual tags).
func stateCondition(s State) string {
	const settled = `p.ocr_status <> 'pending'`
	switch s {
	case StatePreparing:
		return `(p.ocr_status = 'pending' AND p.thumbnail_storage_id IS NULL)`
	case StateDetecting:
		return `(p.ocr_status = 'pending' AND p.thumbnail_storage_id IS NOT NULL)`
	case StateReview:
		return `(` + settled + ` AND t.review > 0)`
	case StateVerified:
		return `(` + settled + ` AND t.review = 0 AND t.n > 0 AND t.manual = t.n)`
	case StateAuto:
		return `(` + settled + ` AND t.review = 0 AND t.n > 0 AND t.manual < t.n)`
	case StateFailed:
		return `(p.ocr_status = 'failed' AND t.n = 0)`
	case StateNoBIB:
		return `(p.ocr_status = 'processed' AND t.n = 0)`
	}
	return ``
}

const photoFrom = `
	FROM photos p
	JOIN albums a ON a.id = p.album_id
	JOIN users u ON u.id = p.created_by
	JOIN objects oo ON oo.id = p.original_storage_id
	LEFT JOIN objects tn ON tn.id = p.thumbnail_storage_id
	LEFT JOIN LATERAL (
		SELECT count(*) AS n,
		       count(*) FILTER (WHERE source = 'ocr' AND COALESCE(confidence_score, 0) < 0.85) AS review,
		       count(*) FILTER (WHERE source = 'manual') AS manual
		FROM photo_tags WHERE photo_id = p.id
	) t ON true`

const photoColumns = `p.id, p.tenant_id, p.album_id, p.original_filename, p.original_size, p.size, p.width, p.height,
	p.ocr_status, p.ocr_error, p.created_by, p.created_at, p.updated_at,
	u.first_name, u.last_name, u.email,
	oo.bucket, oo.object_key, tn.bucket, tn.object_key`

func scanPhoto(row dbutil.RowScanner) (*Photo, error) {
	p := &Photo{}
	var first, last string
	var thumbBucket, thumbKey sql.NullString
	err := row.Scan(&p.ID, &p.TenantID, &p.AlbumID, &p.OriginalFilename, &p.OriginalSize, &p.Size, &p.Width, &p.Height,
		&p.OCRStatus, &p.OCRError, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
		&first, &last, &p.UploaderEmail,
		&p.OriginalBucket, &p.OriginalKey, &thumbBucket, &thumbKey)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	p.UploaderName = strings.TrimSpace(first + " " + last)
	if thumbKey.Valid {
		p.ThumbBucket, p.ThumbKey = &thumbBucket.String, &thumbKey.String
	}
	return p, nil
}

// ListPhotos returns one keyset page of an event's photos, newest first,
// with their tags.
func (r *Repository) ListPhotos(ctx context.Context, tenantID, eventID string, f PhotoFilter, page pagination.PageParams) (pagination.Page[Photo], error) {
	limit := page.NormalizeLimit()
	where := []string{`a.tenant_id = $1`, `a.event_id = $2`}
	args := []any{tenantID, eventID}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.AlbumID != "" {
		where = append(where, `p.album_id = `+arg(f.AlbumID))
	}
	if f.UploaderID != "" {
		where = append(where, `p.created_by = `+arg(f.UploaderID))
	}
	if f.BIB != "" {
		where = append(where, `EXISTS (SELECT 1 FROM photo_tags pt WHERE pt.photo_id = p.id AND pt.bib_string = `+arg(f.BIB)+`)`)
	}
	if cond := stateCondition(f.State); cond != "" {
		where = append(where, cond)
	}
	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		where = append(where, `(p.created_at, p.id) < (`+arg(c.CreatedAt)+`, `+arg(c.ID)+`)`)
	}
	query := `SELECT ` + photoColumns + photoFrom + ` WHERE ` + strings.Join(where, ` AND `) +
		` ORDER BY p.created_at DESC, p.id DESC LIMIT ` + fmt.Sprint(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[Photo]{}, err
	}
	defer rows.Close()
	var photos []Photo
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return pagination.Page[Photo]{}, err
		}
		photos = append(photos, *p)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[Photo]{}, err
	}

	var next string
	if len(photos) > limit {
		last := photos[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		photos = photos[:limit]
	}
	if err := r.attachTags(ctx, tenantID, photos); err != nil {
		return pagination.Page[Photo]{}, err
	}
	return pagination.Page[Photo]{Items: photos, NextCursor: next}, nil
}

// attachTags loads the tags of the given photos in one query.
func (r *Repository) attachTags(ctx context.Context, tenantID string, photos []Photo) error {
	if len(photos) == 0 {
		return nil
	}
	ids := make([]string, len(photos))
	index := make(map[string]int, len(photos))
	for i := range photos {
		ids[i] = photos[i].ID
		index[photos[i].ID] = i
	}
	rows, err := r.db.Q(ctx).QueryContext(ctx, `
		SELECT id, photo_id, bib_string, source, confidence_score, box_x, box_y, box_w, box_h
		FROM photo_tags WHERE tenant_id = $1 AND photo_id = ANY($2::uuid[])
		ORDER BY created_at, id`, tenantID, pq.Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t Tag
		var conf, x, y, w, h sql.NullFloat64
		if err := rows.Scan(&t.ID, &t.PhotoID, &t.BIB, &t.Source, &conf, &x, &y, &w, &h); err != nil {
			return err
		}
		if conf.Valid {
			t.Confidence = &conf.Float64
		}
		if x.Valid && y.Valid && w.Valid && h.Valid {
			t.Box = &Box{X: x.Float64, Y: y.Float64, W: w.Float64, H: h.Float64}
		}
		i := index[t.PhotoID]
		photos[i].Tags = append(photos[i].Tags, t)
	}
	return rows.Err()
}
