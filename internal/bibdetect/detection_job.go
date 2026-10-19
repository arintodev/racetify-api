package bibdetect

import (
    "context"
    "encoding/json"
    "fmt"
    "strings"
    "regexp"
    "strconv"

    "github.com/racetify/racetify-api/internal/domain"
    "github.com/racetify/racetify-api/internal/jobqueue"
    "github.com/racetify/racetify-api/internal/platform/dbutil"
    "github.com/racetify/racetify-api/internal/security"
    "github.com/racetify/racetify-api/internal/gallery"
)

// detectPayload mirrors gallery.thumbnailPayload. It is the shape of the job
// payload used when the gallery's CompleteUploads enqueues a detection job.
type detectPayload struct {
    EventID  string   `json:"event_id"`
    AlbumID  string   `json:"album_id"`
    PhotoIDs []string `json:"photo_ids"`
}

// RunBibDetect processes a bib‑detection job.
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
    var failures []string
    err = s.db.WithTenantTx(ctx, job.TenantID, func(txCtx context.Context) error {
        _ = s.queue.UpdateProgress(ctx, job, 0, total)
        for i, photoID := range p.PhotoIDs {
            if err := txCtx.Err(); err != nil {
                return err
            }
            n, err := s.detectBib(txCtx, job.TenantID, p.EventID, photoID)
            if err != nil {
                failures = append(failures, fmt.Sprintf("%s: %v", photoID, err))
            }
            if n > 0 {
                _ = s.gallery.SetOCRStatus(txCtx, job.TenantID, photoID, gallery.OCRProcessed)
            } else {
                _ = s.gallery.SetOCRFailed(txCtx, job.TenantID, photoID, "no bib detected")
            }
            _ = s.queue.UpdateProgress(ctx, job, i+1, total)
        }
        return nil
    })
    if err != nil {
        return nil, "", err
    }
    result := map[string]any{"count": total}
    if len(failures) > 0 {
        msg := fmt.Sprintf("%d of %d photos could not be BIB-detected", len(failures), total)
        if len(failures) > 0 {
            msg += ": " + strings.Join(failures, "; ")
        }
        return result, msg, nil
    }
    return result, "", nil
}

// detectBib performs the OCR for a single photo and returns the number of tags inserted.
func (s *Service) detectBib(ctx context.Context, tenantID, eventID, photoID string) (int, error) {
    var inserted int
    err := s.db.WithTenantTx(ctx, tenantID, func(txCtx context.Context) error {
        photo, err := s.gallery.GetPhotoByID(txCtx, tenantID, photoID)
        if err != nil {
            return err
        }
        ticket, err := s.storage.RequestDownload(txCtx, tenantID, photo.OriginalKey, photo.OriginalBucket)
        if err != nil {
            return fmt.Errorf("sign original url: %w", err)
        }
        detected, err := s.bibOCR.ProcessURL(txCtx, ticket.URL)
        if err != nil {
            return fmt.Errorf("detect/single: %w", err)
        }
        if len(detected) == 0 {
            return nil
        }
        // Extract the numeric part of the OCR'd text to store as bib_number.
        // We use a simple regex that captures the first run of digits.  If no
        // digits are found, we pass nil to InsertOCRTag which will result in a
        // NULL bib_number in the database.
        re := regexp.MustCompile(`\d+`)
        for _, d := range detected {
            var bibNumPtr *int
            if match := re.FindString(d.Text); match != "" {
                if n, err := strconv.Atoi(match); err == nil {
                    bibNumPtr = &n
                }
            }
            tag := &gallery.Tag{
                ID:         security.MustNewUUIDv4(),
                PhotoID:    photoID,
                BIB:        d.Text,
                Source:     "ocr",
                Confidence: &d.Confidence,
            }
            if err := s.gallery.InsertOCRTag(txCtx, tenantID, tag, bibNumPtr); err != nil {
                if dbutil.IsUniqueViolation(err) {
                    continue
                }
                return fmt.Errorf("insert OCR tag: %w", err)
            }
            inserted++
        }
        return nil
    })
    return inserted, err
}

// RegisterJob registers the RunBibDetect job with the dispatcher.
// It is called from cmd/worker/main.go with the dispatcher and a fully
// initialized bibdetect.Service instance.
func RegisterJob(d *jobqueue.Dispatcher, svc *Service) {
    d.Register(domain.JobTypeMediaBibDetect, func(ctx context.Context, job *jobqueue.Job) (map[string]any, string, error) {
        return svc.RunBibDetect(ctx, job)
    })
}