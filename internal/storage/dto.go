package storage

import "time"

type ObjectDTO struct {
	ID          string    `json:"id"`
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	SHA256      *string   `json:"sha256,omitempty"`
	Status      string    `json:"status"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

func objectResponse(o *Object) ObjectDTO {
	return ObjectDTO{
		ID: o.ID, Bucket: string(o.Bucket), Key: o.ObjectKey, ContentType: o.ContentType,
		SizeBytes: o.SizeBytes, SHA256: o.SHA256, Status: string(o.Status),
		CreatedBy: o.CreatedBy, CreatedAt: o.CreatedAt,
	}
}

// UploadTicketDTO is RequestUpload's response: where to PUT the raw bytes,
// and when that grant expires.
type UploadTicketDTO struct {
	ID        string    `json:"id"`
	UploadURL string    `json:"upload_url"`
	ExpiresAt time.Time `json:"expires_at"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
}

// DownloadTicketDTO is RequestDownload's response. ExpiresAt is omitted for
// a public-bucket object, whose URL is stable and unsigned - see
// Service.RequestDownload.
type DownloadTicketDTO struct {
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}
