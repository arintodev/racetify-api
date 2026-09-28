// Package watermark is the bounded context for an event's watermark
// settings: zero or more image layers composited onto each gallery photo's
// thumbnail (internal/gallery/thumbnail_job.go's applyWatermarks). It
// mirrors internal/gallery and internal/generator's layering (routes.go /
// handler.go / service.go / repository.go / dto.go), and reuses the
// existing Object Storage upload flow (internal/storage) - a watermark
// image is uploaded like any other object, and this package is handed its
// storage_id.
package watermark

import (
	"net/http"
	"time"
)

// Watermark is one image layer configured for an event. Positioning is
// anchor + percent offset (of the thumbnail canvas), sizing is a percent of
// the canvas' diagonal plus an aspect ratio - see thumbnail_job.go's
// applyWatermarks for the exact math that turns these into pixels.
type Watermark struct {
	ID       string
	TenantID string
	EventID  string

	StorageID string
	Name      string

	// AnchorX is one of "left", "center", "right".
	AnchorX string
	// AnchorY is one of "top", "center", "bottom".
	AnchorY string

	OffsetXPercent float64
	OffsetYPercent float64
	WidthPercent   float64
	AspectRatio    float64
	Opacity        float64
	SortOrder      int

	CreatedAt time.Time
	UpdatedAt time.Time

	// Bucket/ObjectKey are the storage object's, joined in by
	// Repository.ListByEvent so Service.PreviewURL can resolve a fetch URL
	// without a second query per watermark (mirrors gallery.Photo's
	// OriginalBucket/OriginalKey).
	Bucket    string
	ObjectKey string
}

// Error is a failure the client can act on: it carries its own HTTP status,
// machine-readable code and the request field it concerns (mirrors
// gallery.Error / generator.Error).
type Error struct {
	Status  int
	Code    string
	Field   string
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(field, message string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Field: field, Message: message}
}

// Valid anchor values (spec: anchor_x in {left,center,right}, anchor_y in
// {top,center,bottom}).
var (
	validAnchorX = map[string]bool{"left": true, "center": true, "right": true}
	validAnchorY = map[string]bool{"top": true, "center": true, "bottom": true}
)

// maxNameLen bounds a watermark layer's display name.
const maxNameLen = 100

// MaxWatermarks bounds one event's set of layers - generous for the actual
// UX (a handful of logos/sponsor marks at most) while keeping ReplaceAll's
// per-item validation loop and the thumbnail job's per-photo compositing
// loop bounded.
const MaxWatermarks = 20
