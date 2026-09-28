package watermark

import "time"

// watermarkItemRequest is one layer of a PUT /watermarks request body. ID is
// omitted (or null) for a newly added layer.
type watermarkItemRequest struct {
	ID             *string `json:"id,omitempty"`
	StorageID      string  `json:"storage_id"`
	Name           string  `json:"name"`
	AnchorX        string  `json:"anchor_x"`
	AnchorY        string  `json:"anchor_y"`
	OffsetXPercent float64 `json:"offset_x_percent"`
	OffsetYPercent float64 `json:"offset_y_percent"`
	WidthPercent   float64 `json:"width_percent"`
	AspectRatio    float64 `json:"aspect_ratio"`
	Opacity        float64 `json:"opacity"`
	SortOrder      int     `json:"sort_order"`
}

type replaceRequest struct {
	Watermarks []watermarkItemRequest `json:"watermarks"`
}

// WatermarkDTO is a watermark layer as the API returns it. PreviewURL is
// resolved from the underlying storage object the same way gallery's
// PhotoURLs / generator's SVGURL resolve a public/presigned URL.
type WatermarkDTO struct {
	ID                  string     `json:"id"`
	EventID             string     `json:"event_id"`
	StorageID           string     `json:"storage_id"`
	Name                string     `json:"name"`
	AnchorX             string     `json:"anchor_x"`
	AnchorY             string     `json:"anchor_y"`
	OffsetXPercent      float64    `json:"offset_x_percent"`
	OffsetYPercent      float64    `json:"offset_y_percent"`
	WidthPercent        float64    `json:"width_percent"`
	AspectRatio         float64    `json:"aspect_ratio"`
	Opacity             float64    `json:"opacity"`
	SortOrder           int        `json:"sort_order"`
	PreviewURL          string     `json:"preview_url"`
	PreviewURLExpiresAt *time.Time `json:"preview_url_expires_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

func (s *Service) watermarkResponse(w *Watermark) (WatermarkDTO, error) {
	url, expires, err := s.PreviewURL(w)
	if err != nil {
		return WatermarkDTO{}, err
	}
	return WatermarkDTO{
		ID: w.ID, EventID: w.EventID, StorageID: w.StorageID, Name: w.Name,
		AnchorX: w.AnchorX, AnchorY: w.AnchorY,
		OffsetXPercent: w.OffsetXPercent, OffsetYPercent: w.OffsetYPercent,
		WidthPercent: w.WidthPercent, AspectRatio: w.AspectRatio, Opacity: w.Opacity,
		SortOrder: w.SortOrder, PreviewURL: url, PreviewURLExpiresAt: expires,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}, nil
}
