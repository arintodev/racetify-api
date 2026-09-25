// Package gallery is the bounded context for an event's media: albums, the
// photos in them and their BIB tags (docs/phase1-api-plan.md §4.4). Routes
// are event-scoped (/events/{id}/...) so they can reuse the event package's
// RequireEventAccess gate, which admits tenant staff or an external crew
// member holding a gallery capability.
package gallery

import (
	"net/http"
	"regexp"
	"time"
)

// Album groups the photos of one event (Finish Line, CP 1, ...). Runners
// only find photos in a public album; a new album is a draft.
type Album struct {
	ID          string
	TenantID    string
	EventID     string
	Name        string
	Description *string
	IsPublic    bool
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// PhotoCount is filled by reads that list albums.
	PhotoCount int
}

// Error is a failure the client can act on: it carries its own HTTP status,
// machine-readable code and the request field it concerns.
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

func conflict(code, field, message string) *Error {
	return &Error{Status: http.StatusConflict, Code: code, Field: field, Message: message}
}

const maxAlbumNameLen = 100

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID guards ids that come from a client before they reach a uuid column,
// where a malformed one would surface as a database error instead of a 400.
func isUUID(s string) bool { return uuidPattern.MatchString(s) }

// Upload rules (docs/media-gallery-ux.md §3.2, §5).
const (
	// MaxBatch bounds one upload-urls / complete request.
	MaxBatch = 50
	// MaxInputBytes is the biggest file a photographer may pick, before the
	// browser resizes it.
	MaxInputBytes = 50 << 20
	// MaxStoredBytes is the biggest file the server keeps, after the resize.
	MaxStoredBytes = 20 << 20
)

// OCR thresholds (§3.4), lower bound inclusive. A tag under the auto
// threshold needs review; the min is what the OCR job discards below.
const (
	OCRAutoThreshold = 0.85
	OCRMinConfidence = 0.40
)

const (
	OCRPending   = "pending"
	OCRProcessed = "processed"
	OCRFailed    = "failed"
)

const (
	TagSourceOCR    = "ocr"
	TagSourceManual = "manual"
)

// Photo is one uploaded picture. The original is the file the browser sent
// (resized to 2048 px), kept in the private bucket; the thumbnail is made
// later by the processing job.
type Photo struct {
	ID               string
	TenantID         string
	AlbumID          string
	OriginalFilename string
	OriginalSize     int64
	Size             int64
	Width            int
	Height           int
	OCRStatus        string
	OCRError         *string
	// CreatedBy is the photographer: always the user who was logged in
	// when uploading.
	CreatedBy     string
	UploaderName  string
	UploaderEmail string
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Where the files live, for building URLs.
	OriginalBucket string
	OriginalKey    string
	ThumbBucket    *string
	ThumbKey       *string

	Tags []Tag
}

// HasThumbnail is thumbnail_storage_id being set.
func (p *Photo) HasThumbnail() bool { return p.ThumbKey != nil }

// Box is where OCR found a tag, as fractions of the photo's width/height.
type Box struct{ X, Y, W, H float64 }

// Tag is a BIB on a photo. Manual tags have no confidence and no box.
type Tag struct {
	ID         string
	PhotoID    string
	BIB        string
	Source     string
	Confidence *float64
	Box        *Box
}

// State is the status shown in the UI, derived from the stored columns
// (§3.3). The first rule that matches wins; the SQL in ListPhotos'
// state filter mirrors this order.
type State string

const (
	StatePreparing State = "preparing"
	StateDetecting State = "detecting"
	StateReview    State = "review"
	StateVerified  State = "verified"
	StateAuto      State = "auto"
	StateFailed    State = "failed"
	StateNoBIB     State = "no_bib"
)

func (s State) Valid() bool {
	switch s {
	case StatePreparing, StateDetecting, StateReview, StateVerified, StateAuto, StateFailed, StateNoBIB:
		return true
	}
	return false
}
