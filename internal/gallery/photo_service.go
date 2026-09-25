package gallery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
)

// acceptedTypes are the formats a photographer may pick (§5), with the
// content type stored for each.
var acceptedTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".heic": "image/heic",
	".heif": "image/heif",
}

// objectKey is where a picked file is stored. It is derived from the album
// and the file's name + size, so asking for an upload URL again for the same
// file after a failed attempt reuses the same object instead of leaving an
// orphan behind for every retry.
func objectKey(albumID, filename string, size int64) string {
	sum := sha256.Sum256([]byte(DuplicateKey(filename, size)))
	return fmt.Sprintf("%s%s%s", albumPrefix(albumID), hex.EncodeToString(sum[:12]), strings.ToLower(path.Ext(filename)))
}

func albumPrefix(albumID string) string { return "photos/" + albumID + "/" }

// UploadEntry is one file a photographer wants to upload.
type UploadEntry struct {
	Filename     string
	OriginalSize int64
}

// UploadResult answers one UploadEntry, in the same order. Skipped is set
// ("duplicate") instead of a URL for a file that is already in the album.
type UploadResult struct {
	StorageID string
	UploadURL string
	ExpiresAt time.Time
	Skipped   string
}

// RequestUploadURLs hands out presigned upload URLs for up to MaxBatch files
// of one album, and reports the ones already there. The caller has been
// authorised (staff, or a crew member holding gallery:upload).
func (s *Service) RequestUploadURLs(ctx context.Context, tenantID, eventID, albumID, actorUserID string, entries []UploadEntry) ([]UploadResult, error) {
	if len(entries) == 0 {
		return nil, invalid("files", "files is required.")
	}
	if len(entries) > MaxBatch {
		return nil, invalid("files", fmt.Sprintf("at most %d files per request.", MaxBatch))
	}
	if !isUUID(albumID) {
		return nil, domain.ErrNotFound
	}
	names := make([]string, len(entries))
	sizes := make([]int64, len(entries))
	for i, e := range entries {
		e.Filename = path.Base(strings.ReplaceAll(e.Filename, `\`, "/"))
		entries[i] = e
		if _, ok := acceptedTypes[strings.ToLower(path.Ext(e.Filename))]; !ok {
			return nil, invalid("files", fmt.Sprintf("%s: format not supported.", e.Filename))
		}
		if e.OriginalSize <= 0 || e.OriginalSize > MaxInputBytes {
			return nil, invalid("files", fmt.Sprintf("%s: size must be between 1 byte and 50 MB.", e.Filename))
		}
		names[i], sizes[i] = e.Filename, e.OriginalSize
	}

	var existing map[string]bool
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if _, err := s.repo.GetAlbum(ctx, tenantID, eventID, albumID); err != nil {
			return err
		}
		var err error
		existing, err = s.repo.DuplicateKeys(ctx, tenantID, albumID, names, sizes)
		return err
	})
	if err != nil {
		return nil, err
	}

	results := make([]UploadResult, len(entries))
	seen := map[string]bool{}
	for i, e := range entries {
		key := DuplicateKey(e.Filename, e.OriginalSize)
		if existing[key] || seen[key] {
			results[i] = UploadResult{Skipped: "duplicate"}
			continue
		}
		seen[key] = true
		ticket, err := s.storage.ProvisionUpload(ctx, tenantID, actorUserID, string(objectstorage.BucketPrivate),
			objectKey(albumID, e.Filename, e.OriginalSize), acceptedTypes[strings.ToLower(path.Ext(e.Filename))])
		if err != nil {
			return nil, err
		}
		results[i] = UploadResult{StorageID: ticket.ObjectID, UploadURL: ticket.UploadURL, ExpiresAt: ticket.ExpiresAt}
	}
	return results, nil
}

// CompleteItem is one file the browser finished uploading.
type CompleteItem struct {
	StorageID        string
	OriginalFilename string
	OriginalSize     int64
	Width            int
	Height           int
}

// Outcome of completing one file.
const (
	CompleteCreated  = "created"
	CompleteExists   = "exists"
	CompleteRejected = "rejected"
)

// CompleteResult answers one CompleteItem, in the same order.
type CompleteResult struct {
	StorageID string
	PhotoID   string
	Status    string
	Reason    string
}

// CompleteUploads turns uploaded objects into photos (ocr_status=pending).
// Completing the same object again returns the photo it already made. The
// thumbnail and OCR job is not queued yet; photos stay pending.
func (s *Service) CompleteUploads(ctx context.Context, tenantID, eventID, albumID, actorUserID string, items []CompleteItem) ([]CompleteResult, error) {
	if len(items) == 0 {
		return nil, invalid("items", "items is required.")
	}
	if len(items) > MaxBatch {
		return nil, invalid("items", fmt.Sprintf("at most %d items per request.", MaxBatch))
	}
	if !isUUID(albumID) {
		return nil, domain.ErrNotFound
	}
	for _, it := range items {
		if !isUUID(it.StorageID) {
			return nil, invalid("items", "storage_id is not valid.")
		}
	}
	if err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		_, err := s.repo.GetAlbum(ctx, tenantID, eventID, albumID)
		return err
	}); err != nil {
		return nil, err
	}

	// Confirm what reached storage first: with the r2 driver this is a
	// HEAD request per object, which should not hold a transaction open.
	objects := make([]*storage.Object, len(items))
	results := make([]CompleteResult, len(items))
	for i, it := range items {
		results[i] = CompleteResult{StorageID: it.StorageID}
		obj, err := s.storage.ConfirmStored(ctx, tenantID, actorUserID, it.StorageID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			results[i].Status, results[i].Reason = CompleteRejected, "not an uploaded file of this workspace"
		case err != nil:
			return nil, err
		case obj.Bucket != storage.ObjectBucketPrivate || !strings.HasPrefix(obj.ObjectKey, albumPrefix(albumID)):
			results[i].Status, results[i].Reason = CompleteRejected, "not an upload of this album"
		case obj.Status != storage.ObjectStatusStored:
			results[i].Status, results[i].Reason = CompleteRejected, "the file was not uploaded"
		case obj.SizeBytes > MaxStoredBytes:
			results[i].Status, results[i].Reason = CompleteRejected, "the file exceeds 20 MB"
		case strings.TrimSpace(it.OriginalFilename) == "" || it.OriginalSize <= 0:
			results[i].Status, results[i].Reason = CompleteRejected, "original_filename and original_size are required"
		default:
			objects[i] = obj
		}
	}

	created := 0
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		now := time.Now().UTC()
		for i, it := range items {
			if objects[i] == nil {
				continue
			}
			p := &Photo{
				ID: security.MustNewUUIDv4(), TenantID: tenantID, AlbumID: albumID, CreatedBy: actorUserID,
				OriginalFilename: path.Base(it.OriginalFilename), OriginalSize: it.OriginalSize,
				Size: objects[i].SizeBytes, Width: it.Width, Height: it.Height, CreatedAt: now,
			}
			inserted, err := s.repo.InsertPhoto(ctx, p, it.StorageID)
			if err != nil {
				return err
			}
			if inserted {
				results[i].PhotoID, results[i].Status = p.ID, CompleteCreated
				created++
				continue
			}
			// Already completed before: hand back the photo it made.
			results[i].Status = CompleteExists
			if id, err := s.repo.PhotoIDByStorage(ctx, tenantID, it.StorageID); err == nil {
				results[i].PhotoID = id
			}
		}
		if created == 0 {
			return nil
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionPhotoUploaded,
			map[string]any{"album_id": albumID, "event_id": eventID, "photos": created})
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// ListPhotos returns one page of an event's photos. A non-empty
// f.UploaderID limits it to that photographer's own uploads.
func (s *Service) ListPhotos(ctx context.Context, tenantID, eventID string, f PhotoFilter, page pagination.PageParams) (pagination.Page[Photo], error) {
	if f.State != "" && !f.State.Valid() {
		return pagination.Page[Photo]{}, invalid("state", "state is not valid.")
	}
	if f.AlbumID != "" && !isUUID(f.AlbumID) {
		return pagination.Page[Photo]{}, invalid("album_id", "album_id is not valid.")
	}
	var out pagination.Page[Photo]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListPhotos(ctx, tenantID, eventID, f, page)
		return err
	})
	return out, err
}

// PhotoURLs returns where to fetch a photo. Preview is the thumbnail when
// the job made one, else the original, so the grid shows something as soon
// as the upload is complete. Original is always the stored 2048 px file.
func (s *Service) PhotoURLs(p *Photo) (preview, original string, err error) {
	ticket, err := s.store.PresignDownload(objectstorage.Bucket(p.OriginalBucket), p.TenantID, p.OriginalKey, s.cfg.DownloadTTL)
	if err != nil {
		return "", "", err
	}
	original = ticket.URL
	if p.ThumbBucket == nil || p.ThumbKey == nil {
		return original, original, nil
	}
	if objectstorage.Bucket(*p.ThumbBucket) == objectstorage.BucketPublic {
		return s.store.PublicURL(p.TenantID, *p.ThumbKey), original, nil
	}
	thumb, err := s.store.PresignDownload(objectstorage.Bucket(*p.ThumbBucket), p.TenantID, *p.ThumbKey, s.cfg.DownloadTTL)
	if err != nil {
		return "", "", err
	}
	return thumb.URL, original, nil
}
