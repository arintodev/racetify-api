package face

import (
	"context"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/gallery"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/platform/qdrantstore"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
)

const (
	sourceDetection  = "detection"
	sourceEnrollment = "enrollment"
)

// Service implements the face search use cases. It depends directly on
// *gallery.Service (not an interface) for GetPhotoByID/ListPhotos/
// PhotoResponse - face imports gallery, never the reverse, so this is not
// an import cycle (gallery only needs the domain.JobTypeMediaFaceDetect
// constant to enqueue the detection job, from internal/domain, not this
// package - see gallery/photo_service.go's CompleteUploads).
type Service struct {
	db      *database.DB
	repo    *Repository
	audit   *audit.Repository
	storage *storage.Service
	qdrant  *qdrantstore.Store
	embed   *EmbedClient
	gallery *gallery.Service
	// queue enqueues media.face_detect jobs. Nil is tolerated the same way
	// gallery.Service.queue is - a caller that only needs read paths (some
	// tests) just gets no job enqueued.
	queue *jobqueue.Queue
}

func NewService(db *database.DB, repo *Repository, auditRepo *audit.Repository, storageService *storage.Service,
	qdrant *qdrantstore.Store, embed *EmbedClient, galleryService *gallery.Service, queue *jobqueue.Queue) *Service {
	return &Service{
		db: db, repo: repo, audit: auditRepo, storage: storageService,
		qdrant: qdrant, embed: embed, gallery: galleryService, queue: queue,
	}
}

func (s *Service) recordAudit(ctx context.Context, tenantID, actorUserID, action string, metadata map[string]any) error {
	return s.audit.Record(ctx, &audit.Log{
		ID:          security.MustNewUUIDv4(),
		TenantID:    &tenantID,
		ActorUserID: &actorUserID,
		Action:      action,
		Metadata:    metadata,
		CreatedAt:   time.Now().UTC(),
	})
}

// ==================== enrollment ====================

// Enroll adds one embedding, produced from imageBytes, under userID's face
// for this event - creating the faces row on first enrollment. consent must
// be true or the request is refused outright (consentRequired) before any
// embedding is even computed; the image itself is never persisted anywhere
// (docs/face-search-plan.md's Privacy section) - it exists only as
// imageBytes, in memory, for the duration of this call.
func (s *Service) Enroll(ctx context.Context, tenantID, eventID, userID, actorUserID string, imageBytes []byte, filename string, consent bool) (*Face, error) {
	if !consent {
		return nil, consentRequired()
	}
	if len(imageBytes) == 0 {
		return nil, invalid("image", "image is required.")
	}

	faces, err := s.embed.DetectAndEmbed(ctx, imageBytes, filename)
	if err != nil {
		return nil, err
	}
	if len(faces) == 0 {
		return nil, invalid("image", "No face was detected in this image.")
	}
	// Multiple faces in one enrollment image is ambiguous (whose face is
	// being enrolled?) - use the highest-confidence detection, the same
	// "pick the best candidate" shape gallery's OCR auto-threshold uses,
	// rather than rejecting the upload outright.
	best := faces[0]
	for _, f := range faces[1:] {
		if f.Confidence > best.Confidence {
			best = f
		}
	}

	var face *Face
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		existing, err := s.repo.GetFaceByUser(ctx, tenantID, eventID, userID)
		switch {
		case err == nil:
			face = existing
		case err == domain.ErrNotFound:
			now := time.Now().UTC()
			face = &Face{
				ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID, UserID: userID,
				ConsentedAt: now, CreatedAt: now, UpdatedAt: now,
			}
			if err := s.repo.InsertFace(ctx, face); err != nil {
				return err
			}
		default:
			return err
		}

		pointID := security.MustNewUUIDv4()
		confidence := best.Confidence
		embedding := &Embedding{
			ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID, UserID: userID,
			FaceID: face.ID, QdrantPointID: pointID, ConfidenceScore: &confidence, CreatedAt: time.Now().UTC(),
		}
		if err := s.repo.InsertEmbedding(ctx, embedding); err != nil {
			return err
		}
		if err := s.repo.IncrementEmbeddingCount(ctx, tenantID, face.ID); err != nil {
			return err
		}
		face.EmbeddingCount++

		if err := s.qdrant.Upsert(ctx, qdrantstore.Point{
			ID: pointID, Vector: best.Embedding,
			Payload: qdrantstore.Payload{TenantID: tenantID, EventID: eventID, Source: sourceEnrollment, UserID: userID, FaceID: face.ID},
		}); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, "face.enrolled",
			map[string]any{"face_id": face.ID, "event_id": eventID, "user_id": userID})
	})
	if err != nil {
		return nil, err
	}
	return face, nil
}

// ListEmbeddings returns a user's enrolled face and its embedding records
// (metadata only - never an image, never the vector itself) for an event.
// Returns (nil, nil, nil) if the user has not enrolled.
func (s *Service) ListEmbeddings(ctx context.Context, tenantID, eventID, userID string) (*Face, []Embedding, error) {
	var face *Face
	var embeddings []Embedding
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		f, err := s.repo.GetFaceByUser(ctx, tenantID, eventID, userID)
		if err == domain.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		face = f
		embeddings, err = s.repo.ListEmbeddingsByFace(ctx, tenantID, f.ID)
		return err
	})
	return face, embeddings, err
}

// RevokeConsent withdraws consent for a face_id and actively deletes its
// stored biometric data: every face_embeddings row and the Qdrant points
// they point at (docs/face-search-plan.md's Privacy section - this is the
// "right to erasure" path, not just a flag left for later cleanup).
func (s *Service) RevokeConsent(ctx context.Context, tenantID, faceID, actorUserID string) error {
	var pointIDs []string
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if _, err := s.repo.GetFace(ctx, tenantID, faceID); err != nil {
			return err
		}
		if err := s.repo.RevokeConsent(ctx, tenantID, faceID); err != nil {
			return err
		}
		ids, err := s.repo.DeleteEmbeddingsByFace(ctx, tenantID, faceID)
		if err != nil {
			return err
		}
		pointIDs = ids
		return s.recordAudit(ctx, tenantID, actorUserID, "face.consent_revoked", map[string]any{"face_id": faceID})
	})
	if err != nil {
		return err
	}
	// Best-effort outside the DB transaction, mirroring how other Qdrant
	// calls in this package are not themselves transactional with Postgres
	// (qdrantstore.Store.do doc comment) - a failure here leaves orphaned
	// Qdrant points (harmless, since nothing in Postgres references them
	// any more) rather than blocking the consent withdrawal itself.
	return s.qdrant.Delete(ctx, pointIDs)
}

// ==================== search ====================

// Search resolves faceID's enrolled embeddings, finds gallery photos whose
// detected faces are similar to at least one of them, and delegates the
// actual paginated listing to gallery.Service.ListPhotos (reusing its
// pagination/DTO/URL-signing) via a PhotoIDs filter -
// docs/face-search-plan.md's recommended approach, mirroring how BIB search
// already composes with ListPhotos.
func (s *Service) Search(ctx context.Context, tenantID, eventID, faceID string, page pagination.PageParams) (pagination.Page[gallery.Photo], error) {
	var face *Face
	var embeddings []Embedding
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		f, err := s.repo.GetFace(ctx, tenantID, faceID)
		if err != nil {
			return err
		}
		if f.EventID != eventID {
			return domain.ErrNotFound
		}
		face = f
		embeddings, err = s.repo.ListEmbeddingsByFace(ctx, tenantID, faceID)
		return err
	})
	if err != nil {
		return pagination.Page[gallery.Photo]{}, err
	}
	if face.Revoked() {
		return pagination.Page[gallery.Photo]{}, consentRevoked()
	}
	if len(embeddings) == 0 {
		return pagination.Page[gallery.Photo]{}, invalid("face_id", "This face has no enrolled embeddings.")
	}

	// One Qdrant search per enrolled embedding, keeping the best score per
	// matching photo_id across all of them - docs/face-search-plan.md's
	// Search section (no server-side "average embedding" step; a person's
	// enrolled photos can look different enough across angles/lighting that
	// the best single match per photo is a more forgiving signal than an
	// average vector would be).
	bestScore := map[string]float64{}
	for _, e := range embeddings {
		vector, err := s.pointVector(ctx, e.QdrantPointID)
		if err != nil {
			continue // a missing/unreadable point should not fail the whole search
		}
		results, err := s.qdrant.Search(ctx, vector,
			qdrantstore.Filter{TenantID: tenantID, EventID: eventID, Source: sourceDetection}, searchResultLimit)
		if err != nil {
			return pagination.Page[gallery.Photo]{}, err
		}
		for _, r := range results {
			if r.Score < SimilarityThreshold || r.Payload.PhotoID == "" {
				continue
			}
			if cur, ok := bestScore[r.Payload.PhotoID]; !ok || r.Score > cur {
				bestScore[r.Payload.PhotoID] = r.Score
			}
		}
	}
	if len(bestScore) == 0 {
		return pagination.Page[gallery.Photo]{}, nil
	}
	photoIDs := make([]string, 0, len(bestScore))
	for id := range bestScore {
		photoIDs = append(photoIDs, id)
	}

	return s.gallery.ListPhotos(ctx, tenantID, eventID, gallery.PhotoFilter{PhotoIDs: photoIDs}, page)
}

// pointVector reads a single enrolled embedding's vector back out of Qdrant
// by point id. Vectors are never cached in Postgres (only qdrant_point_id
// is stored there) - this is fine since a query vector is only ever needed
// at search time, for a user's own small number of enrolled embeddings.
func (s *Service) pointVector(ctx context.Context, pointID string) ([]float32, error) {
	return s.qdrant.GetVector(ctx, pointID)
}

// PhotoResponse exposes gallery.Service.PhotoResponse to face.Handler,
// which only has a *face.Service, not the gallery service directly.
func (s *Service) PhotoResponse(p *gallery.Photo) (gallery.PhotoDTO, error) {
	return s.gallery.PhotoResponse(p)
}
