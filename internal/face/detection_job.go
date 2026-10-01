// Face detection for uploaded gallery photos (docs/face-search-plan.md,
// racetify-app repo), modeled directly on
// internal/gallery/thumbnail_job.go: gallery.Service.CompleteUploads
// enqueues one media.face_detect job per batch of newly created photos,
// using the same payload shape thumbnail_job.go's own media.photo_process
// job uses (event/album/photo ids only - no personal data). RunFaceDetect
// (this file) downloads each original, sends it to the face-embedding
// microservice, and records what it found - it never assigns a face_id to
// what it detects (see face.go's package doc comment for why).
package face

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/platform/qdrantstore"
	"github.com/racetify/racetify-api/internal/security"
)

// detectPayload mirrors gallery's thumbnailPayload exactly - both jobs are
// enqueued from the same CompleteUploads batch, so they share this shape.
type detectPayload struct {
	EventID  string   `json:"event_id"`
	AlbumID  string   `json:"album_id"`
	PhotoIDs []string `json:"photo_ids"`
}

// RegisterJob registers the worker's handler for face detection. Singular,
// matching gallery.RegisterJob/bibprint.RegisterJob/certificate.RegisterJob
// - this package registers exactly one domain.JobType.
func RegisterJob(d *jobqueue.Dispatcher, svc *Service) {
	d.Register(domain.JobTypeMediaFaceDetect, svc.RunFaceDetect)
}

// RunFaceDetect is the media.face_detect handler: it detects faces in each
// photo of the job's payload and stores what it found. One photo failing
// (a corrupt file, an embed-service error) is recorded as failed and does
// not stop the rest - the same partial-batch-tolerant shape
// gallery.Service.RunPhotoProcess uses.
func (s *Service) RunFaceDetect(ctx context.Context, job *jobqueue.Job) (map[string]any, string, error) {
	raw, err := json.Marshal(job.Payload)
	if err != nil {
		return nil, "", err
	}
	var p detectPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, "", fmt.Errorf("face: bad payload: %w", err)
	}
	if len(p.PhotoIDs) == 0 {
		return nil, "", fmt.Errorf("face: %w: photo_ids is empty", domain.ErrInvalidState)
	}
	if !s.embed.Configured() {
		return nil, "", fmt.Errorf("face: FACE_EMBED_SERVICE_URL is not configured")
	}

	total := len(p.PhotoIDs)
	_ = s.queue.UpdateProgress(ctx, job, 0, total)

	made := 0
	skipped := 0
	failed := 0
	var failures []string
	for i, photoID := range p.PhotoIDs {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		switch n, err := s.detectPhoto(ctx, job.TenantID, p.EventID, photoID); {
		case err != nil:
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", photoID, err))
		case n == 0:
			skipped++
		default:
			made++
		}
		_ = s.queue.UpdateProgress(ctx, job, i+1, total)
	}

	result := map[string]any{"count": total, "made": made, "skipped": skipped, "failed": failed}
	if failed > 0 {
		msg := fmt.Sprintf("%d of %d photos could not be face-detected", failed, total)
		if len(failures) > 0 {
			msg += ": " + strings.Join(failures, "; ")
		}
		return result, msg, nil
	}
	return result, "", nil
}

// detectPhoto detects faces in one photo and records each as a
// photo_face_detections row plus an unassigned Qdrant point. Returns the
// number of faces found (0 is a graceful skip, not an error - matching
// thumbnailOutcome's shape in thumbnail_job.go).
func (s *Service) detectPhoto(ctx context.Context, tenantID, eventID, photoID string) (int, error) {
	photo, err := s.gallery.GetPhotoByID(ctx, tenantID, photoID)
	if err != nil {
		return 0, err
	}
	// The face-detector service fetches the image itself, so it needs a
	// URL, not bytes (embedclient.go's own doc comment) - the original
	// already lives in the private bucket as a tracked object, so this is
	// just a short-lived signed download URL, the same one PhotoURLs/
	// PreviewURL build for an authorized viewer.
	ticket, err := s.storage.RequestDownload(ctx, tenantID, photo.OriginalKey, photo.OriginalBucket)
	if err != nil {
		return 0, fmt.Errorf("sign original url: %w", err)
	}
	detected, err := s.embed.DetectAndEmbed(ctx, photoID, ticket.URL)
	if err != nil {
		return 0, fmt.Errorf("detect/single: %w", err)
	}
	if len(detected) == 0 {
		return 0, nil
	}

	now := time.Now().UTC()
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		for _, f := range detected {
			pointID := security.MustNewUUIDv4()
			confidence := f.Confidence
			box := boxFraction(f.BBoxPixel, photo.Width, photo.Height)
			d := &Detection{
				ID: security.MustNewUUIDv4(), TenantID: tenantID, PhotoID: photoID,
				BoxX: box.X, BoxY: box.Y, BoxW: box.W, BoxH: box.H,
				ConfidenceScore: &confidence, QdrantPointID: pointID, DetectedAt: now,
			}
			if err := s.repo.InsertDetection(ctx, d); err != nil {
				return err
			}
			if err := s.qdrant.Upsert(ctx, qdrantstore.Point{
				ID: pointID, Vector: f.Embedding,
				Payload: qdrantstore.Payload{TenantID: tenantID, EventID: eventID, Source: sourceDetection, PhotoID: photoID},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(detected), nil
}
