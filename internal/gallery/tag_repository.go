package gallery

import (
	"context"
	"strconv"
	"strings"

	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// parseBibNumber pulls the digits-only reading out of a BIB string (§4.C:
// "1024" out of "A-1024"), or nil when it has none - the same shape
// bib_number already has for OCR-made rows (migrations/0009_media_gallery).
// A manually typed BIB gets the same treatment so it is reachable through
// either half of the frictionless search's OR clause.
func parseBibNumber(bib string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(bib))
	if err != nil {
		return nil
	}
	return &n
}

// translateTagWrite turns a taken (photo_id, bib_string) pair
// (photo_tags_photo_bib_uk, migrations/0016_gallery_extend) into a conflict
// the client can show under the bib field.
func translateTagWrite(err error) error {
	if dbutil.IsUniqueViolation(err) {
		return conflict("tag_bib_exists", "bib", "This photo already has a tag with this BIB.")
	}
	return err
}

// InsertTag adds a manual tag. Manual tags never carry a confidence or a
// box (dto.go's TagDTO/photoResponse).
func (r *Repository) InsertTag(ctx context.Context, tenantID string, t *Tag) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO photo_tags (id, tenant_id, photo_id, bib_string, bib_number, source, confidence_score, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULL,now())`,
		t.ID, tenantID, t.PhotoID, t.BIB, parseBibNumber(t.BIB), t.Source)
	return translateTagWrite(err)
}

// InsertOCRTag records one OCR-read BIB with its confidence and, when the
// detector supplied one, its box (docs/media-gallery-integration.md). The
// photo bib service's own response carries no box (ocrclient.go's
// DetectedBIB doc comment), so t.Box is nil for every tag runOCR inserts
// today - box_x/y/w/h go in as NULL, same as a manual tag, rather than
// being a required field. bibNumber is passed in rather than derived here
// via parseBibNumber (which requires the whole string to be numeric, so it
// would reject e.g. "A1024") - runOCR's ocrBibFields extracts it from the
// raw OCR text directly, matching the photo bib service's own reference
// client. Unlike InsertTag, a photo_tags_photo_bib_uk violation here is
// NOT translated to a conflict.Error - the caller (Service.runOCR) treats
// a duplicate BIB on the same photo as a graceful skip (a manual tag or an
// earlier OCR read already claimed it), never a job failure.
func (r *Repository) InsertOCRTag(ctx context.Context, tenantID string, t *Tag, bibNumber *int) error {
	var boxX, boxY, boxW, boxH *float64
	if t.Box != nil {
		boxX, boxY, boxW, boxH = &t.Box.X, &t.Box.Y, &t.Box.W, &t.Box.H
	}
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO photo_tags (id, tenant_id, photo_id, bib_string, bib_number, source, confidence_score, box_x, box_y, box_w, box_h, created_at)
		VALUES ($1,$2,$3,$4,$5,'ocr',$6,$7,$8,$9,$10,now())`,
		t.ID, tenantID, t.PhotoID, t.BIB, bibNumber, t.Confidence, boxX, boxY, boxW, boxH)
	return err
}

// UpdateTagBIB is both "confirm" (bib unchanged) and "correct" (bib
// changed): an OCR tag becomes a manual one either way, keeping its box so
// the inspector can still point at where it was found, but losing its
// confidence, which only ever described the machine's own reading
// (docs/media-gallery-integration.md).
func (r *Repository) UpdateTagBIB(ctx context.Context, tenantID, photoID, tagID, bib string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE photo_tags SET bib_string = $4, bib_number = $5, source = 'manual', confidence_score = NULL
		WHERE tenant_id = $1 AND photo_id = $2 AND id = $3`,
		tenantID, photoID, tagID, bib, parseBibNumber(bib))
	if err != nil {
		return translateTagWrite(err)
	}
	return dbutil.CheckRowsAffected(res)
}

// DeleteTag removes one tag.
func (r *Repository) DeleteTag(ctx context.Context, tenantID, photoID, tagID string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`DELETE FROM photo_tags WHERE tenant_id = $1 AND photo_id = $2 AND id = $3`, tenantID, photoID, tagID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// SetOCRStatus flips a photo's derived-state input directly (used to mark a
// still-pending photo processed the moment a human adds or confirms a tag
// on it, docs/media-gallery-integration.md's "Keputusan" note - and by
// Service.runOCR once its own detection pass finishes).
func (r *Repository) SetOCRStatus(ctx context.Context, tenantID, photoID, status string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE photos SET ocr_status = $3 WHERE tenant_id = $1 AND id = $2`, tenantID, photoID, status)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// SetOCRFailed marks a photo's OCR pass as failed and records why -
// Service.runOCR's counterpart to SetOCRStatus(..., OCRProcessed) for the
// success path.
func (r *Repository) SetOCRFailed(ctx context.Context, tenantID, photoID, errMsg string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE photos SET ocr_status = 'failed', ocr_error = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, photoID, errMsg)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}
