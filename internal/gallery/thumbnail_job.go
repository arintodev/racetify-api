// Thumbnail generation and BIB OCR for uploaded gallery photos (Phase 1,
// this file only - see docs/media-gallery-integration.md for what is and
// isn't built). CompleteUploads queues one media.photo_process job per
// batch of newly created photos; RunPhotoProcess (this file) does two
// independent things per photo, in the same job (not two job types, unlike
// internal/face's media.face_detect: OCR is a per-photo enrichment step of
// this package's own "process this photo" pipeline - it only ever writes
// this package's own photo_tags - whereas face detection populates a
// wholly separate subsystem, internal/face's Qdrant-backed vector search,
// that this package deliberately does not import):
//  1. downloads the original, resizes it and stores the result, so
//     ListPhotos/PhotoURLs stop falling back to the full-size original
//     once it finishes (makeThumbnail).
//  2. hands the photo bib service (the externally-deployed BIB-detection
//     microservice) a downloadable URL for the original and records what
//     it reads as photo_tags rows (runOCR) - gracefully skipped, not
//     failed, when config.BibOCRConfig.ServiceURL is unset, so
//     thumbnailing never depends on this service's availability.
//
// Watermarking (applyWatermarks, below) composites an event's configured
// watermark.Watermark layers (internal/watermark) onto each thumbnail,
// loaded once per batch (loadWatermarks) rather than once per photo.
//
// Explicitly NOT built here (docs/phase1-api-plan.md §6/§9's later
// phases, both flagged to the user and deferred on purpose):
//   - Watermarking the original (only the thumbnail is watermarked).
//   - "re-run-ocr" (re-processing an already-settled photo on demand).
package gallery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // decode support only; this job never writes PNGs
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/image/draw"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/watermark"
)

// Thumbnail dimensions/format (§ "Implement" step 1 of the task that added
// this file):
//   - ThumbnailMaxEdge=640px longest edge. The client already resizes the
//     original down to gallery.go's RESIZE_MAX_EDGE-equivalent (2048px,
//     see apps/dashboard/src/lib/gallery.ts) for the full-size view; a
//     grid tile never needs anywhere near that, and 480-640px is the usual
//     range for a masonry/grid thumbnail. 640 was picked (not 480) to stay
//     sharp on 2x/3x device pixel ratios for a ~200-300 CSS px tile.
//   - ThumbnailJPEGQuality=82. The browser's own resize uses 90
//     (RESIZE_QUALITY in gallery.ts) for the archival original; a
//     thumbnail is redrawn at a much smaller resolution and viewed small,
//     so a somewhat lower quality is imperceptible there and keeps the
//     grid's total payload down.
const (
	ThumbnailMaxEdge     = 640
	ThumbnailJPEGQuality = 82
)

// heicExtensions are formats the upload flow accepts (see
// apps/dashboard/src/lib/gallery.ts's ACCEPTED_EXTENSIONS) that this job
// cannot decode: Go's standard library has no HEIC/HEIF decoder, and
// pulling in libheif (cgo) or a large pure-Go decoder is a real dependency
// decision this task does not make on its own. A HEIC/HEIF photo is left
// without a thumbnail on purpose - PhotoURLs already falls back to the
// original in that case - rather than failing the job or adding a heavy
// new dependency.
var heicExtensions = map[string]bool{".heic": true, ".heif": true}

// thumbnailPayload is a media.photo_process job's payload: the photos one
// CompleteUploads call created. No personal data - just ids, like
// bibprint/certificate's batch payloads.
type thumbnailPayload struct {
	EventID  string   `json:"event_id"`
	AlbumID  string   `json:"album_id"`
	PhotoIDs []string `json:"photo_ids"`
}

// RegisterJob registers the worker's handler for gallery photo processing
// (thumbnail generation only, today - see this file's package doc comment).
// Singular RegisterJob, not RegisterJobs: gallery registers exactly one
// domain.JobType (media.photo_process), the same "one call per module" shape
// bibprint.RegisterJob/certificate.RegisterJob already use. Watermarking and
// OCR, when they land, are additional work *within* this same job type
// (a photo's processing pipeline), not new job types of their own, so
// RegisterJob is expected to stay accurate rather than needing to become
// plural later.
func RegisterJob(d *jobqueue.Dispatcher, svc *Service) {
	d.Register(domain.JobTypeMediaPhotoProcess, svc.RunPhotoProcess)
}

// enqueueThumbnailJob queues one media.photo_process job for the given,
// just-created photo ids.
func (s *Service) enqueueThumbnailJob(ctx context.Context, tenantID, eventID, albumID, actorUserID string, photoIDs []string) (string, error) {
	raw, err := json.Marshal(thumbnailPayload{EventID: eventID, AlbumID: albumID, PhotoIDs: photoIDs})
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	return s.queue.Enqueue(ctx, tenantID, actorUserID, domain.JobTypeMediaPhotoProcess, payload)
}

// RunPhotoProcess is the media.photo_process handler: it makes a thumbnail
// for each photo in the job's payload. One photo failing (a corrupt file,
// an unsupported format that isn't HEIC/HEIF) is recorded as failed and
// does not stop the rest, the same "partial batch" shape
// certificate.RunBatch and bibprint.Service.Run's own per-item loops use.
func (s *Service) RunPhotoProcess(ctx context.Context, job *jobqueue.Job) (map[string]any, string, error) {
	raw, err := json.Marshal(job.Payload)
	if err != nil {
		return nil, "", err
	}
	var p thumbnailPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, "", fmt.Errorf("gallery: bad payload: %w", err)
	}
	if len(p.PhotoIDs) == 0 {
		return nil, "", fmt.Errorf("gallery: %w: photo_ids is empty", domain.ErrInvalidState)
	}

	total := len(p.PhotoIDs)
	_ = s.queue.UpdateProgress(ctx, job, 0, total)

	// Load the event's watermark configs and decode each referenced source
	// image once for the whole batch, not once per photo - makeThumbnail
	// below just looks assets up by storage id.
	watermarks, assets, err := s.loadWatermarks(ctx, job.TenantID, p.EventID)
	if err != nil {
		return nil, "", fmt.Errorf("gallery: load watermarks: %w", err)
	}

	// The OCR step is skipped for the whole batch, not attempted and
	// failed per photo, when unconfigured - thumbnailing must not depend
	// on this service's availability (this file's package doc comment).
	ocrConfigured := s.ocr != nil && s.ocr.Configured()

	made, skipped, failed := 0, 0, 0
	bibMade, bibSkipped, bibFailed := 0, 0, 0
	var failures, bibFailures []string
	for i, photoID := range p.PhotoIDs {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		switch outcome, err := s.makeThumbnail(ctx, job.TenantID, job.CreatedBy, photoID, watermarks, assets); {
		case err != nil:
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", photoID, err))
		case outcome == thumbnailSkipped:
			skipped++
		default:
			made++
		}

		if !ocrConfigured {
			bibSkipped++
		} else {
			switch outcome, err := s.runOCR(ctx, job.TenantID, photoID); {
			case err != nil:
				bibFailed++
				bibFailures = append(bibFailures, fmt.Sprintf("%s: %v", photoID, err))
			case outcome == ocrSkipped:
				bibSkipped++
			default:
				bibMade++
			}
		}

		_ = s.queue.UpdateProgress(ctx, job, i+1, total)
	}

	result := map[string]any{
		"count": total,
		"made": made, "skipped": skipped, "failed": failed,
		"bib_made": bibMade, "bib_skipped": bibSkipped, "bib_failed": bibFailed,
	}
	var msgs []string
	if failed > 0 {
		msg := fmt.Sprintf("%d of %d photos could not be thumbnailed", failed, total)
		if len(failures) > 0 {
			msg += ": " + strings.Join(failures, "; ")
		}
		msgs = append(msgs, msg)
	}
	if bibFailed > 0 {
		msg := fmt.Sprintf("%d of %d photos could not be BIB-detected", bibFailed, total)
		if len(bibFailures) > 0 {
			msg += ": " + strings.Join(bibFailures, "; ")
		}
		msgs = append(msgs, msg)
	}
	return result, strings.Join(msgs, "; "), nil
}

type thumbnailOutcome int

const (
	thumbnailMade thumbnailOutcome = iota
	// thumbnailSkipped is not a failure: an unsupported-for-decoding format
	// (HEIC/HEIF) or a photo that already has a thumbnail (a retried job).
	thumbnailSkipped
)

// loadWatermarks loads the event's configured watermark layers and decodes
// each distinct referenced source image once, for a whole job batch (see
// RunPhotoProcess) rather than once per photo. A layer whose source cannot
// be read or decoded is left out of the returned map - applyWatermarks
// skips it gracefully, the same "graceful skip" style makeThumbnail already
// uses for a HEIC original. s.watermarks == nil (a caller that does not
// wire the accessor, e.g. some tests) is tolerated: it just means no
// watermarks are ever composited.
func (s *Service) loadWatermarks(ctx context.Context, tenantID, eventID string) ([]*watermark.Watermark, map[string]image.Image, error) {
	if s.watermarks == nil {
		return nil, nil, nil
	}
	var list []*watermark.Watermark
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		list, err = s.watermarks.ListByEvent(ctx, tenantID, eventID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	assets := make(map[string]image.Image, len(list))
	for _, wm := range list {
		if _, ok := assets[wm.StorageID]; ok {
			continue
		}
		data, err := s.storage.ReadObject(ctx, tenantID, wm.StorageID)
		if err != nil {
			continue
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			continue
		}
		assets[wm.StorageID] = img
	}
	return list, assets, nil
}

// makeThumbnail does the work for one photo: load its row, download the
// original, decode/resize/re-encode it, store the result, and record it.
// watermarks/assets are the whole batch's watermark configs and decoded
// source images (loadWatermarks), shared across every photo in the job.
func (s *Service) makeThumbnail(ctx context.Context, tenantID, actorUserID, photoID string, watermarks []*watermark.Watermark, assets map[string]image.Image) (thumbnailOutcome, error) {
	var p *Photo
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		p, err = s.repo.GetPhotoByID(ctx, tenantID, photoID)
		return err
	})
	if err != nil {
		return 0, err
	}
	if p.HasThumbnail() {
		return thumbnailSkipped, nil
	}
	if heicExtensions[strings.ToLower(path.Ext(p.OriginalFilename))] {
		// Graceful, non-error skip - see this file's heicExtensions doc
		// comment. PhotoURLs already falls back to the original.
		return thumbnailSkipped, nil
	}

	data, err := s.storage.ReadObject(ctx, tenantID, p.OriginalStorageID)
	if err != nil {
		return 0, fmt.Errorf("read original: %w", err)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("decode image: %w", err)
	}

	thumb := resizeToMaxEdge(src, ThumbnailMaxEdge)
	thumb = applyWatermarks(thumb, watermarks, assets)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: ThumbnailJPEGQuality}); err != nil {
		return 0, fmt.Errorf("encode thumbnail: %w", err)
	}

	key := thumbnailKey(p.AlbumID, p.ID)
	obj, err := s.storage.StoreGenerated(ctx, tenantID, actorUserID, string(objectstorage.BucketPrivate), key, "image/jpeg", buf.Bytes())
	if err != nil {
		return 0, fmt.Errorf("store thumbnail: %w", err)
	}
	if err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.repo.UpdatePhotoThumbnail(ctx, tenantID, p.ID, obj.ID)
	}); err != nil {
		return 0, fmt.Errorf("record thumbnail: %w", err)
	}
	return thumbnailMade, nil
}

type ocrOutcome int

const (
	ocrMade ocrOutcome = iota
	// ocrSkipped is not a failure: a photo whose ocr_status is no longer
	// "pending" (already processed/failed - a retried job, or a human
	// already tagged/confirmed it and settlePending moved it past
	// pending), the same "already done, leave it" reasoning
	// thumbnailSkipped uses for HasThumbnail().
	ocrSkipped
)

var (
	nonAlnumPattern = regexp.MustCompile(`[^a-zA-Z0-9]`)
	nonDigitPattern = regexp.MustCompile(`[^0-9]`)
)

// ocrBibFields turns one raw OCR text reading into the (bib_string,
// bib_number) pair photo_tags stores, matching the photo bib service's own
// reference client exactly rather than reusing tag_repository.go's
// parseBibNumber (which requires the whole string to already be numeric,
// so it would reject e.g. "A1024"): bib_string keeps only letters/digits,
// case preserved; bib_number is the digits-only reading, or nil when the
// text has none. An empty bib_string (the reading was pure punctuation/
// whitespace) is the caller's signal to discard it - there is nothing a
// person could confirm or search for.
func ocrBibFields(text string) (bibString string, bibNumber *int) {
	bibString = nonAlnumPattern.ReplaceAllString(text, "")
	digits := nonDigitPattern.ReplaceAllString(text, "")
	if digits == "" {
		return bibString, nil
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return bibString, nil
	}
	return bibString, &n
}

// runOCR does the work for one photo: load its row, skip it if it is not
// still ocr_status=pending, otherwise hand the photo bib service a
// downloadable URL for the original and record every reading above
// OCRMinConfidence as a photo_tags row before marking the photo processed.
// A reading below OCRMinConfidence is discarded outright - too unreliable
// to even surface for manual review (gallery.go's OCRMinConfidence doc
// comment).
func (s *Service) runOCR(ctx context.Context, tenantID, photoID string) (ocrOutcome, error) {
	var p *Photo
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		p, err = s.repo.GetPhotoByID(ctx, tenantID, photoID)
		return err
	})
	if err != nil {
		return 0, err
	}
	if p.OCRStatus != OCRPending {
		return ocrSkipped, nil
	}

	// The photo bib service fetches the image itself, so it needs a URL,
	// not bytes - unlike internal/face.EmbedClient, which is handed the
	// bytes directly. The original lives in the private bucket, so this is
	// a short-lived signed download URL (objectstorage.GetURL), the same
	// one PhotoURLs/PreviewURL build for an authorized viewer.
	ticket, err := objectstorage.GetURL(s.store, objectstorage.Bucket(p.OriginalBucket), tenantID, p.OriginalKey, s.cfg.DownloadTTL)
	if err != nil {
		return 0, fmt.Errorf("sign original url: %w", err)
	}
	detected, err := s.ocr.ProcessURL(ctx, ticket.URL)
	if err != nil {
		// Best-effort: a failure to even flag the photo as failed must not
		// mask the original process-url error returned below.
		_ = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
			return s.repo.SetOCRFailed(ctx, tenantID, photoID, err.Error())
		})
		return 0, fmt.Errorf("process-url: %w", err)
	}

	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		for _, d := range detected {
			if d.Confidence < OCRMinConfidence {
				continue
			}
			bibString, bibNumber := ocrBibFields(d.Text)
			if bibString == "" {
				continue
			}
			confidence := d.Confidence
			tag := &Tag{ID: security.MustNewUUIDv4(), PhotoID: photoID, BIB: bibString, Source: TagSourceOCR, Confidence: &confidence}
			if err := s.repo.InsertOCRTag(ctx, tenantID, tag, bibNumber); err != nil {
				if dbutil.IsUniqueViolation(err) {
					// A manual tag or an earlier OCR read already claimed
					// this exact BIB on this photo (photo_tags_photo_bib_uk)
					// - not a failure, just nothing new to add.
					continue
				}
				return err
			}
		}
		return s.repo.SetOCRStatus(ctx, tenantID, photoID, OCRProcessed)
	})
	if err != nil {
		return 0, err
	}
	return ocrMade, nil
}

// thumbnailKey is where a photo's thumbnail is stored: alongside its
// original, under the same album prefix objectKey uses, so both live in
// one place per album.
func thumbnailKey(albumID, photoID string) string {
	return fmt.Sprintf("%sthumb/%s.jpg", albumPrefix(albumID), photoID)
}

// resizeToMaxEdge scales src down so its longest edge is at most maxEdge,
// preserving aspect ratio, using golang.org/x/image/draw's CatmullRom
// scaler (a high-quality resampler, worth the extra cost here since a
// thumbnail job runs once per photo, off the request path). A photo
// already at or under maxEdge on both dimensions is returned unchanged -
// there is no reason to re-encode (and potentially lose quality on) an
// already-small image.
func resizeToMaxEdge(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return src
	}
	var newW, newH int
	if w >= h {
		newW = maxEdge
		newH = int(float64(h) * float64(maxEdge) / float64(w))
	} else {
		newH = maxEdge
		newW = int(float64(w) * float64(maxEdge) / float64(h))
	}
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// resizeExact scales src to exactly w x h (no aspect-ratio preservation -
// the caller, applyWatermarks, has already computed w/h from the
// watermark's own aspect ratio), using the same CatmullRom scaler
// resizeToMaxEdge uses. It returns an *image.NRGBA rather than plain
// image.Image because applyWatermarks needs direct pixel access afterwards
// to multiply in the layer's opacity.
func resizeExact(src image.Image, w, h int) *image.NRGBA {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

// applyOpacity multiplies img's alpha channel by opacity in place (newAlpha
// = uint8(float64(oldAlpha) * opacity)). opacity >= 1 is a no-op.
func applyOpacity(img *image.NRGBA, opacity float64) {
	if opacity >= 1 {
		return
	}
	if opacity < 0 {
		opacity = 0
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = uint8(float64(img.Pix[i]) * opacity)
	}
}

// applyWatermarks composites an event's configured watermark layers onto
// img (the already-resized thumbnail) and returns the result. It is a pure
// function - no I/O, no database or storage access, everything it needs
// (watermarks, assets) is loaded once per job batch by loadWatermarks - so
// it stays a separate, independently testable function from
// makeThumbnail's resize/encode pipeline. len(watermarks)==0 returns img
// unchanged, so an event with no watermarks configured pays zero overhead.
//
// Sizing math (widthPercent is a fraction of the canvas' diagonal, so a
// watermark keeps a stable apparent size across differently-shaped photos):
//
//	canvasDiagonal = hypot(imageWidth, imageHeight)
//	watermarkDiagonal = widthPercent * canvasDiagonal
//	denom = sqrt(1 + 1/aspectRatio^2)
//	watermarkWidth = round(watermarkDiagonal / denom)
//	watermarkHeight = round(watermarkWidth / aspectRatio)
//
// Positioning is anchor + a percent-of-canvas offset from that anchor's
// edge (or the center, for anchor "center").
func applyWatermarks(img image.Image, watermarks []*watermark.Watermark, assets map[string]image.Image) image.Image {
	if len(watermarks) == 0 {
		return img
	}
	b := img.Bounds()
	dst := image.NewNRGBA(b)
	draw.Draw(dst, b, img, b.Min, draw.Src)

	imageWidth, imageHeight := float64(b.Dx()), float64(b.Dy())
	canvasDiagonal := math.Hypot(imageWidth, imageHeight)

	for _, wm := range watermarks {
		asset, ok := assets[wm.StorageID]
		if !ok {
			// Graceful skip - same style as thumbnail_job.go's HEIC
			// handling: an unreadable/undecodable source must not fail the
			// whole photo, let alone the batch.
			continue
		}
		if wm.AspectRatio <= 0 || wm.WidthPercent <= 0 {
			continue
		}

		watermarkDiagonal := wm.WidthPercent * canvasDiagonal
		denom := math.Sqrt(1 + 1/(wm.AspectRatio*wm.AspectRatio))
		watermarkWidth := int(math.Round(watermarkDiagonal / denom))
		watermarkHeight := int(math.Round(float64(watermarkWidth) / wm.AspectRatio))
		if watermarkWidth < 1 || watermarkHeight < 1 {
			continue
		}

		offsetX := int(math.Round(imageWidth * wm.OffsetXPercent))
		offsetY := int(math.Round(imageHeight * wm.OffsetYPercent))

		var left, top int
		switch wm.AnchorX {
		case "left":
			left = offsetX
		case "center":
			left = (b.Dx()-watermarkWidth)/2 + offsetX
		default: // "right"
			left = b.Dx() - watermarkWidth - offsetX
		}
		switch wm.AnchorY {
		case "top":
			top = offsetY
		case "center":
			top = (b.Dy()-watermarkHeight)/2 + offsetY
		default: // "bottom"
			top = b.Dy() - watermarkHeight - offsetY
		}

		resized := resizeExact(asset, watermarkWidth, watermarkHeight)
		applyOpacity(resized, wm.Opacity)
		draw.Draw(dst, image.Rect(left, top, left+watermarkWidth, top+watermarkHeight), resized, image.Point{}, draw.Over)
	}
	return dst
}
