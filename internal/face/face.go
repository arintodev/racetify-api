// Package face is the bounded context for face search over an event's media
// gallery (docs/face-search-plan.md, racetify-app repo): detecting faces in
// gallery photos, letting a user enroll their own face (one or more
// embeddings under a single face_id), and searching gallery photos by an
// enrolled face.
//
// A face_id is a user-enrolled identity, and only that - it is never
// created by clustering faces detected in gallery photos. Detected faces
// (photo_face_detections) are plain, unassigned detector output: no
// clustering step ever runs against them, and they carry no face_id column
// at all. Matching happens at search time: an enrolled face_id's embeddings
// are compared, in Qdrant, against the pool of detected embeddings, and the
// best-matching photos are returned. This mirrors the gallery package's
// shape (face.go/dto.go/handler.go/service.go/repository.go/routes.go,
// plus detection_job.go for the background job) and reuses
// gallery.Service.ListPhotos for the actual photo listing once a search has
// resolved a set of matching photo ids - see service.go's Search.
package face

import (
	"net/http"
	"regexp"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID guards ids that come from a client before they reach a uuid
// column - same helper as gallery.isUUID, duplicated rather than shared
// since it's a two-line regexp check, not worth a cross-package import for.
func isUUID(s string) bool { return uuidPattern.MatchString(s) }

// Error is a failure the client can act on: it carries its own HTTP status,
// machine-readable code and the request field it concerns - the same shape
// as gallery.Error.
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

func forbidden(code, message string) *Error {
	return &Error{Status: http.StatusForbidden, Code: code, Message: message}
}

// consentRequired is returned by Enroll when the request does not carry an
// explicit consent flag - a faces row (and therefore any face_embeddings)
// must never be created without it (docs/face-search-plan.md's Privacy
// section).
func consentRequired() *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: "consent_required",
		Message: "Consent is required to enroll a face."}
}

// consentRevoked is returned by Search when the target face_id's consent
// has been withdrawn.
func consentRevoked() *Error {
	return forbidden("consent_revoked", "This face's consent has been revoked.")
}

// SimilarityThreshold is the cosine-similarity cutoff a detection point must
// clear, against at least one of an enrolled face's embeddings, to count as
// a match in Search. Documented as a single retunable constant since it
// will need empirical tuning against the chosen embedding model
// (docs/face-search-plan.md).
const SimilarityThreshold = 0.55

// EmbeddingDimension is the embedding model's output vector length
// (512 for ArcFace/InsightFace, per docs/face-search-plan.md) - the size
// qdrantstore.EnsureCollection creates the collection with.
const EmbeddingDimension = 512

// searchResultLimit bounds how many detection points Search asks Qdrant for
// per enrolled embedding, before deduping by photo id.
const searchResultLimit = 200

// Face is one user's enrolled identity for an event.
type Face struct {
	ID               string
	TenantID         string
	EventID          string
	UserID           string
	EmbeddingCount   int
	ConsentedAt      time.Time
	ConsentRevokedAt *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Revoked reports whether this face's consent has been withdrawn.
func (f *Face) Revoked() bool { return f.ConsentRevokedAt != nil }

// Embedding is one enrollment record - never an image, only the metadata
// that points at the vector stored in Qdrant.
type Embedding struct {
	ID              string
	TenantID        string
	EventID         string
	UserID          string
	FaceID          string
	QdrantPointID   string
	ConfidenceScore *float64
	CreatedAt       time.Time
}

// Detection is one detected face instance in a gallery photo. No face_id:
// see this file's package doc comment for why.
type Detection struct {
	ID              string
	TenantID        string
	PhotoID         string
	BoxX, BoxY      float64
	BoxW, BoxH      float64
	ConfidenceScore *float64
	QdrantPointID   string
	DetectedAt      time.Time
}
