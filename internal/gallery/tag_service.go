package gallery

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/security"
)

// validateBIB mirrors validateAlbumName: trim, require non-empty, bound the
// length. §5 has no normalisation rule for a BIB (ListPhotos' bib filter
// matches it exactly as typed), so nothing else is done to it here.
func validateBIB(bib string) (string, *Error) {
	bib = strings.TrimSpace(bib)
	if bib == "" {
		return "", invalid("bib", "bib is required.")
	}
	if utf8.RuneCountInString(bib) > maxBIBLen {
		return "", invalid("bib", "bib is too long.")
	}
	return bib, nil
}

// settlePending flips a still-pending photo to processed the moment a human
// adds or confirms a tag on it (docs/media-gallery-integration.md's
// "Keputusan": a manually reviewed photo must read as reviewed, not get
// overwritten by a later OCR job run).
func (s *Service) settlePending(ctx context.Context, tenantID string, p *Photo) error {
	if p.OCRStatus != OCRPending {
		return nil
	}
	return s.repo.SetOCRStatus(ctx, tenantID, p.ID, OCRProcessed)
}

// AddTag adds a manual tag to a photo (Staff+, or a crew member holding
// gallery:review - enforced by the route gate, the same as the upload
// endpoints; there is no role left to re-check here for an external crew
// member). Adding a BIB the photo already carries is a conflict, whether
// that existing tag is the same manual one or an OCR read of the same BIB.
func (s *Service) AddTag(ctx context.Context, tenantID, eventID, photoID, actorUserID, bibRaw string) (*Photo, error) {
	if !isUUID(photoID) {
		return nil, domain.ErrNotFound
	}
	bib, verr := validateBIB(bibRaw)
	if verr != nil {
		return nil, verr
	}
	var photo *Photo
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		p, err := s.repo.GetPhoto(ctx, tenantID, eventID, photoID)
		if err != nil {
			return err
		}
		tag := &Tag{ID: security.MustNewUUIDv4(), PhotoID: photoID, BIB: bib, Source: TagSourceManual}
		if err := s.repo.InsertTag(ctx, tenantID, tag); err != nil {
			return err
		}
		if err := s.settlePending(ctx, tenantID, p); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionPhotoTagAdded,
			map[string]any{"photo_id": photoID, "event_id": eventID, "bib": bib}); err != nil {
			return err
		}
		photo, err = s.repo.GetPhoto(ctx, tenantID, eventID, photoID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return photo, nil
}

// UpdateTag sets a tag's bib. This is both the UX's "confirm" (bib sent back
// unchanged) and "correct" (bib changed) actions: either way the tag becomes
// manual and loses its OCR confidence (gallery-store.ts's confirmTag and
// updateTag do the same thing to the mock's tag).
func (s *Service) UpdateTag(ctx context.Context, tenantID, eventID, photoID, tagID, actorUserID, bibRaw string) (*Photo, error) {
	if !isUUID(photoID) || !isUUID(tagID) {
		return nil, domain.ErrNotFound
	}
	bib, verr := validateBIB(bibRaw)
	if verr != nil {
		return nil, verr
	}
	var photo *Photo
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		p, err := s.repo.GetPhoto(ctx, tenantID, eventID, photoID)
		if err != nil {
			return err
		}
		if err := s.repo.UpdateTagBIB(ctx, tenantID, photoID, tagID, bib); err != nil {
			return err
		}
		if err := s.settlePending(ctx, tenantID, p); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionPhotoTagCorrected,
			map[string]any{"photo_id": photoID, "tag_id": tagID, "event_id": eventID, "bib": bib}); err != nil {
			return err
		}
		photo, err = s.repo.GetPhoto(ctx, tenantID, eventID, photoID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return photo, nil
}

// DeleteTag removes one tag from a photo.
func (s *Service) DeleteTag(ctx context.Context, tenantID, eventID, photoID, tagID, actorUserID string) (*Photo, error) {
	if !isUUID(photoID) || !isUUID(tagID) {
		return nil, domain.ErrNotFound
	}
	var photo *Photo
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		// Confirms the photo is this event's before touching its tags.
		if _, err := s.repo.GetPhoto(ctx, tenantID, eventID, photoID); err != nil {
			return err
		}
		if err := s.repo.DeleteTag(ctx, tenantID, photoID, tagID); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionPhotoTagRemoved,
			map[string]any{"photo_id": photoID, "tag_id": tagID, "event_id": eventID}); err != nil {
			return err
		}
		var err error
		photo, err = s.repo.GetPhoto(ctx, tenantID, eventID, photoID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return photo, nil
}
