package bibdetect

import (
    "context"
    "encoding/json"
    "fmt"
    "strings"

    "github.com/racetify/racetify-api/internal/domain"
    "github.com/racetify/racetify-api/internal/jobqueue"
)

// detectPayload mirrors gallery.thumbnailPayload. It is the shape of the job
// payload used when the gallery's CompleteUploads enqueues a detection job.
type detectPayload struct {
    EventID  string   `json:"event_id"`
    AlbumID  string   `json:"album_id"`
    PhotoIDs []string `json:"photo_ids"`
}

func RegisterJob(d *jobqueue.Dispatcher, svc *Service) {
	d.Register(domain.JobTypeMediaBibDetect, svc.RunBibDetect)
}

// RunBibDetect processes a BIB detection job. It iterates over each photo ID
// in the payload and runs the OCR detection via gallery.Service.RunOCR.
// The job returns a map containing counts of processed photos and any failures.
func (s *Service) RunBibDetect(ctx context.Context, job *jobqueue.Job) (map[string]any, string, error) {
    raw, err := json.Marshal(job.Payload)
    if err != nil {
        return nil, "", err
    }
    var p detectPayload
    if err := json.Unmarshal(raw, &p); err != nil {
        return nil, "", fmt.Errorf("bibdetect: bad payload: %w", err)
    }
    if len(p.PhotoIDs) == 0 {
        return nil, "", fmt.Errorf("bibdetect: %w: photo_ids is empty", domain.ErrInvalidState)
    }
    total := len(p.PhotoIDs)
    s.queue.UpdateProgress(ctx, job, 0, total)
    var failures []string
    for i, photoID := range p.PhotoIDs {
        if err := ctx.Err(); err != nil {
            return nil, "", err
        }
        // Use the new detectPhoto helper to perform OCR on the photo.
        if _, err := s.detectBib(ctx, job.TenantID, p.EventID, photoID); err != nil {
            failures = append(failures, fmt.Sprintf("%s: %v", photoID, err))
        }
        s.queue.UpdateProgress(ctx, job, i+1, total)
    }
    result := map[string]any{"count": total}
    if len(failures) > 0 {
        msg := fmt.Sprintf("%d of %d photos could not be BIB-detected", len(failures), total)
        msg += ": " + strings.Join(failures, "; ")
        return result, msg, nil
    }
    return result, "", nil
}

func (s *Service) detectBib(ctx context.Context, tenantID, eventID, photoID string) (int, error) {
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
	detected, err := s.bibOCR.ProcessURL(ctx, ticket.URL)
	if err != nil {
		return 0, fmt.Errorf("detect/single: %w", err)
	}
	if len(detected) == 0 {
		return 0, nil
	}

	// save detected bib to database
    return len(detected), nil

}
