package gallery

import "time"

type AlbumDTO struct {
	ID          string    `json:"id"`
	EventID     string    `json:"event_id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	IsPublic    bool      `json:"is_public"`
	PhotoCount  int       `json:"photo_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func albumResponse(a *Album) AlbumDTO {
	return AlbumDTO{
		ID: a.ID, EventID: a.EventID, Name: a.Name, Description: a.Description,
		IsPublic: a.IsPublic, PhotoCount: a.PhotoCount, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

type uploadFileRequest struct {
	Filename     string `json:"filename"`
	OriginalSize int64  `json:"original_size"`
}

type uploadURLsRequest struct {
	Files []uploadFileRequest `json:"files"`
}

// UploadURLDTO answers one requested file, in request order: an upload URL,
// or skipped="duplicate".
type UploadURLDTO struct {
	Filename  string     `json:"filename"`
	StorageID string     `json:"storage_id,omitempty"`
	UploadURL string     `json:"upload_url,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Skipped   string     `json:"skipped,omitempty"`
}

type completeItemRequest struct {
	StorageID        string `json:"storage_id"`
	OriginalFilename string `json:"original_filename"`
	OriginalSize     int64  `json:"original_size"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
}

type completeRequest struct {
	Items []completeItemRequest `json:"items"`
}

type CompleteItemDTO struct {
	StorageID string `json:"storage_id"`
	PhotoID   string `json:"photo_id,omitempty"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

// CompleteDTO is the answer to /complete. job_id is null until the
// thumbnail and OCR job exists; then it is what /jobs/{id} polls.
type CompleteDTO struct {
	Items []CompleteItemDTO `json:"items"`
	JobID *string           `json:"job_id"`
}

type BoxDTO struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type TagDTO struct {
	ID         string   `json:"id"`
	BIB        string   `json:"bib"`
	Source     string   `json:"source"`
	Confidence *float64 `json:"confidence"`
	Box        *BoxDTO  `json:"box"`
}

type UploaderDTO struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type PhotoDTO struct {
	ID               string      `json:"id"`
	AlbumID          string      `json:"album_id"`
	OriginalFilename string      `json:"original_filename"`
	OriginalSize     int64       `json:"original_size"`
	Size             int64       `json:"size"`
	Width            int         `json:"width"`
	Height           int         `json:"height"`
	UploadedBy       UploaderDTO `json:"uploaded_by"`
	UploadedAt       time.Time   `json:"uploaded_at"`
	OCRStatus        string      `json:"ocr_status"`
	OCRError         *string     `json:"ocr_error"`
	HasThumbnail     bool        `json:"has_thumbnail"`
	// PreviewURL is the thumbnail, or the original until one exists.
	PreviewURL string `json:"preview_url"`
	// OriginalURL is the stored 2048 px file (private, presigned).
	OriginalURL string   `json:"original_url"`
	Tags        []TagDTO `json:"tags"`
}

func (s *Service) photoResponse(p *Photo) (PhotoDTO, error) {
	preview, original, err := s.PhotoURLs(p)
	if err != nil {
		return PhotoDTO{}, err
	}
	tags := make([]TagDTO, len(p.Tags))
	for i, t := range p.Tags {
		tags[i] = TagDTO{ID: t.ID, BIB: t.BIB, Source: t.Source, Confidence: t.Confidence}
		if t.Box != nil {
			tags[i].Box = &BoxDTO{X: t.Box.X, Y: t.Box.Y, W: t.Box.W, H: t.Box.H}
		}
	}
	return PhotoDTO{
		ID: p.ID, AlbumID: p.AlbumID, OriginalFilename: p.OriginalFilename, OriginalSize: p.OriginalSize,
		Size: p.Size, Width: p.Width, Height: p.Height,
		UploadedBy: UploaderDTO{ID: p.CreatedBy, Name: p.UploaderName, Email: p.UploaderEmail},
		UploadedAt: p.CreatedAt, OCRStatus: p.OCRStatus, OCRError: p.OCRError, HasThumbnail: p.HasThumbnail(),
		PreviewURL: preview, OriginalURL: original, Tags: tags,
	}, nil
}

type createAlbumRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

type updateAlbumRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	IsPublic    *bool   `json:"is_public"`
}
