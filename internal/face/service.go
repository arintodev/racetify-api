package face

import (
	"context"
	"errors"
	"net/http"
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

// tempImageKey names a short-lived enrollment temp object - unique per
// call, deleted again by the returned cleanup func regardless of whether
// its signed URL (config.StorageConfig.DownloadTTL, the same TTL every
// other download URL in this codebase uses) has expired yet.
func tempImageKey() string { return "face-enroll-tmp/" + security.MustNewUUIDv4() }

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

// recordAudit mirrors oauthclient.Service's own recordAudit shape: exactly
// one of actorUserID/actorClientID is set depending on which path called
// it, and tenantID is nil for a Racetify-user-owned face (recorded outside
// any tenant transaction, the same as a platform-level audit_logs row).
func (s *Service) recordAudit(ctx context.Context, tenantID, actorUserID, actorClientID *string, action string, metadata map[string]any) error {
	return s.audit.Record(ctx, &audit.Log{
		ID:            security.MustNewUUIDv4(),
		TenantID:      tenantID,
		ActorUserID:   actorUserID,
		ActorClientID: actorClientID,
		Action:        action,
		Metadata:      metadata,
		CreatedAt:     time.Now().UTC(),
	})
}

// ==================== enrollment ====================

// detectBestFace runs the embed service against photoURL and picks the
// highest-confidence detection - multiple faces in one enrollment image is
// ambiguous (whose face is being enrolled?), so this uses the same "pick
// the best candidate" shape gallery's OCR auto-threshold uses, rather than
// rejecting the upload outright.
func (s *Service) detectBestFace(ctx context.Context, photoURL string) (*DetectedFace, error) {
	faces, err := s.embed.DetectAndEmbed(ctx, "enroll-"+security.MustNewUUIDv4(), photoURL)
	if err != nil {
		return nil, err
	}
	if len(faces) == 0 {
		return nil, invalid("image", "No face was detected in this image.")
	}
	best := faces[0]
	for _, f := range faces[1:] {
		if f.Confidence > best.Confidence {
			best = f
		}
	}
	return &best, nil
}

// tempUploadOwn writes imageBytes to a personal (no-tenant) temp object
// scoped to userID via created_by, so the externally-deployed
// face-detector service (URL-based, not upload-based - embedclient.go's
// own doc comment) can fetch it. Used only by the self-enroll path
// (Enroll) - a Racetify account's own face has no tenant to scope a
// tracked object under (docs/face-tenant-enrollment-plan.md). The image
// itself is never durably persisted: it exists as this one object for at
// most the few seconds this call takes, and cleanup deletes it
// unconditionally once the caller is done, success or failure.
// StoreGeneratedPersonal/DeletePersonal/RequestDownloadPersonal all work on
// any objectstorage.DirectWriter driver, so this works on STORAGE_DRIVER=r2
// too, not just "local".
func (s *Service) tempUploadOwn(ctx context.Context, userID string, imageBytes []byte) (photoURL string, cleanup func(), err error) {
	key := tempImageKey()
	if _, err := s.storage.StoreGeneratedPersonal(ctx, userID, string(storage.ObjectBucketPrivate), key, http.DetectContentType(imageBytes), imageBytes); err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = s.storage.DeletePersonal(context.Background(), userID, string(storage.ObjectBucketPrivate), key) }
	ticket, err := s.storage.RequestDownloadPersonal(ctx, userID, key, string(storage.ObjectBucketPrivate))
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return ticket.URL, cleanup, nil
}

// tempUploadByRef is tempUploadOwn's tenant M2M counterpart: the temp
// object is tenant-scoped (a ref-based face always belongs to exactly one
// tenant), with no created_by user - the actor is an OAuth client, not a
// Racetify account (migrations/0023 made objects.created_by nullable for
// exactly this pairing). Uses StoreGeneratedDirect rather than the general
// StoreGenerated so this works on STORAGE_DRIVER=r2 too, not just "local".
func (s *Service) tempUploadByRef(ctx context.Context, tenantID string, imageBytes []byte) (photoURL string, cleanup func(), err error) {
	key := tempImageKey()
	if _, err := s.storage.StoreGeneratedDirect(ctx, tenantID, "", string(storage.ObjectBucketPrivate), key, http.DetectContentType(imageBytes), imageBytes); err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = s.storage.DeleteGenerated(context.Background(), tenantID, string(storage.ObjectBucketPrivate), key) }
	ticket, err := s.storage.RequestDownload(ctx, tenantID, key, string(storage.ObjectBucketPrivate))
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return ticket.URL, cleanup, nil
}

// Enroll adds one embedding, produced from imageBytes, under userID's own
// global face - creating the faces row on first enrollment. consent must
// be true or the request is refused outright (consentRequired) before any
// embedding is even computed. Unlike the original design (docs/face-
// search-plan.md's Privacy section), imageBytes is briefly written to a
// temp object so the externally-deployed face-detector service can fetch
// it by URL (tempUploadOwn) - it is deleted again immediately after this
// call returns, success or failure, rather than never touching storage at
// all. The database work itself runs in db.WithTx: a Racetify user's face
// has no tenant (docs/face-tenant-enrollment-plan.md §2.1).
func (s *Service) Enroll(ctx context.Context, userID, actorUserID string, imageBytes []byte, consent bool) (*Face, error) {
	if !consent {
		return nil, consentRequired()
	}
	if len(imageBytes) == 0 {
		return nil, invalid("image", "image is required.")
	}
	photoURL, cleanup, err := s.tempUploadOwn(ctx, userID, imageBytes)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	best, err := s.detectBestFace(ctx, photoURL)
	if err != nil {
		return nil, err
	}

	var face *Face
	err = s.db.WithTx(ctx, func(ctx context.Context) error {
		existing, err := s.repo.GetFaceByUser(ctx, userID)
		switch {
		case err == nil:
			face = existing
		case errors.Is(err, domain.ErrNotFound):
			now := time.Now().UTC()
			face = &Face{ID: security.MustNewUUIDv4(), UserID: &userID, ConsentedAt: now, CreatedAt: now, UpdatedAt: now}
			if err := s.repo.InsertFace(ctx, face); err != nil {
				return err
			}
		default:
			return err
		}

		pointID := security.MustNewUUIDv4()
		confidence := best.Confidence
		embedding := &Embedding{
			ID: security.MustNewUUIDv4(), UserID: &userID,
			FaceID: face.ID, QdrantPointID: pointID, ConfidenceScore: &confidence, CreatedAt: time.Now().UTC(),
		}
		if err := s.repo.InsertEmbedding(ctx, embedding); err != nil {
			return err
		}
		if err := s.repo.IncrementEmbeddingCount(ctx, face.ID); err != nil {
			return err
		}
		face.EmbeddingCount++

		if err := s.qdrant.Upsert(ctx, qdrantstore.Point{
			ID: pointID, Vector: best.Embedding,
			Payload: qdrantstore.Payload{Source: sourceEnrollment, UserID: userID, FaceID: face.ID},
		}); err != nil {
			return err
		}
		return s.recordAudit(ctx, nil, &actorUserID, nil, "face.enrolled",
			map[string]any{"face_id": face.ID, "user_id": userID})
	})
	if err != nil {
		return nil, err
	}
	return face, nil
}

// EnrollByRef is Enroll's tenant M2M counterpart: adds one embedding
// under a face keyed by the tenant's own opaque refID rather than a
// Racetify user_id. Runs in db.WithTenantTx(tenantID, ...) - a ref-based
// face always belongs to exactly one tenant. consentReference, if given,
// is stored only in the audit metadata: it is the tenant's own pointer
// into their own consent record, never verified by Racetify (docs/face-
// tenant-enrollment-plan.md §6).
func (s *Service) EnrollByRef(ctx context.Context, tenantID, refID, actorClientID string,
	imageBytes []byte, consent bool, consentReference string) (*Face, error) {
	if !consent {
		return nil, consentRequired()
	}
	if !isValidRefID(refID) {
		return nil, invalid("ref_id", "ref_id must be 1-200 characters of letters, digits, '_', '.', ':' or '-'.")
	}
	if len(imageBytes) == 0 {
		return nil, invalid("image", "image is required.")
	}
	photoURL, cleanup, err := s.tempUploadByRef(ctx, tenantID, imageBytes)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	best, err := s.detectBestFace(ctx, photoURL)
	if err != nil {
		return nil, err
	}

	var face *Face
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		existing, err := s.repo.GetFaceByRef(ctx, tenantID, refID)
		switch {
		case err == nil:
			face = existing
		case errors.Is(err, domain.ErrNotFound):
			now := time.Now().UTC()
			face = &Face{ID: security.MustNewUUIDv4(), TenantID: &tenantID, RefID: &refID, ConsentedAt: now, CreatedAt: now, UpdatedAt: now}
			if err := s.repo.InsertFace(ctx, face); err != nil {
				return err
			}
		default:
			return err
		}

		pointID := security.MustNewUUIDv4()
		confidence := best.Confidence
		embedding := &Embedding{
			ID: security.MustNewUUIDv4(), TenantID: &tenantID, RefID: &refID,
			FaceID: face.ID, QdrantPointID: pointID, ConfidenceScore: &confidence, CreatedAt: time.Now().UTC(),
		}
		if err := s.repo.InsertEmbedding(ctx, embedding); err != nil {
			return err
		}
		if err := s.repo.IncrementEmbeddingCount(ctx, face.ID); err != nil {
			return err
		}
		face.EmbeddingCount++

		if err := s.qdrant.Upsert(ctx, qdrantstore.Point{
			ID: pointID, Vector: best.Embedding,
			Payload: qdrantstore.Payload{TenantID: tenantID, Source: sourceEnrollment, RefID: refID, FaceID: face.ID},
		}); err != nil {
			return err
		}
		meta := map[string]any{"face_id": face.ID, "ref_id": refID}
		if consentReference != "" {
			meta["consent_reference"] = consentReference
		}
		return s.recordAudit(ctx, &tenantID, nil, &actorClientID, "face.enrolled", meta)
	})
	if err != nil {
		return nil, err
	}
	return face, nil
}

// ListEmbeddings returns a Racetify user's own enrolled face and its
// embedding records (metadata only - never an image, never the vector
// itself). Returns (nil, nil, nil) if the user has not enrolled.
func (s *Service) ListEmbeddings(ctx context.Context, userID string) (*Face, []Embedding, error) {
	var face *Face
	var embeddings []Embedding
	err := s.db.WithTx(ctx, func(ctx context.Context) error {
		f, err := s.repo.GetFaceByUser(ctx, userID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		face = f
		embeddings, err = s.repo.ListEmbeddingsByFace(ctx, f.ID)
		return err
	})
	return face, embeddings, err
}

// ListEmbeddingsByRef is ListEmbeddings' tenant M2M counterpart.
func (s *Service) ListEmbeddingsByRef(ctx context.Context, tenantID, refID string) (*Face, []Embedding, error) {
	var face *Face
	var embeddings []Embedding
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		f, err := s.repo.GetFaceByRef(ctx, tenantID, refID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		face = f
		embeddings, err = s.repo.ListEmbeddingsByFace(ctx, f.ID)
		return err
	})
	return face, embeddings, err
}

// dropFace is the shared hard-delete implementation behind DropOwnFace/
// DropFaceByRef: fetch the face, refuse (as ErrNotFound, not Forbidden -
// no need to reveal whether a face_id the caller doesn't own even exists)
// if ownerCheck rejects it, then delete it and its embeddings from
// Postgres and Qdrant. Must run inside the caller's own transaction
// (db.WithTx or db.WithTenantTx) so the GetFace lookup is scoped
// correctly by RLS.
func (s *Service) dropFace(ctx context.Context, faceID string, ownerCheck func(*Face) bool,
	actorUserID, actorClientID *string) error {
	f, err := s.repo.GetFace(ctx, faceID)
	if err != nil {
		return err
	}
	if !ownerCheck(f) {
		return domain.ErrNotFound
	}
	pointIDs, err := s.repo.DropFace(ctx, faceID)
	if err != nil {
		return err
	}
	if err := s.recordAudit(ctx, f.TenantID, actorUserID, actorClientID, "face.dropped",
		map[string]any{"face_id": faceID}); err != nil {
		return err
	}
	// Best-effort outside the DB transaction, mirroring every other Qdrant
	// call in this package (qdrantstore.Store.do's doc comment) - a
	// failure here leaves orphaned Qdrant points (harmless, since nothing
	// in Postgres references them any more) rather than blocking the drop.
	return s.qdrant.Delete(ctx, pointIDs)
}

// DropOwnFace hard-deletes actorUserID's own global face - the faces row,
// every face_embeddings row under it, and their Qdrant points - freeing
// the account to re-enroll later.
func (s *Service) DropOwnFace(ctx context.Context, actorUserID, faceID string) error {
	return s.db.WithTx(ctx, func(ctx context.Context) error {
		return s.dropFace(ctx, faceID, func(f *Face) bool {
			return f.TenantID == nil && f.UserID != nil && *f.UserID == actorUserID
		}, &actorUserID, nil)
	})
}

// DropFaceByRef hard-deletes a tenant's ref-based face, freeing that
// ref_id for re-enrollment. ownerCheck rejects a global (Racetify-user-
// owned) face outright - an M2M client must never be able to touch one,
// even though the relaxed RLS policy (docs/face-tenant-enrollment-
// plan.md §2.3) would otherwise let the row through.
func (s *Service) DropFaceByRef(ctx context.Context, tenantID, actorClientID, faceID string) error {
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.dropFace(ctx, faceID, func(f *Face) bool {
			return f.RefID != nil
		}, nil, &actorClientID)
	})
}

// ==================== search ====================

// Search resolves faceID's enrolled embeddings, finds gallery photos whose
// detected faces are similar to at least one of them, and delegates the
// actual paginated listing to gallery.Service.ListPhotos (reusing its
// pagination/DTO/URL-signing) via a PhotoIDs filter -
// docs/face-search-plan.md's recommended approach, mirroring how BIB search
// already composes with ListPhotos.
//
// faceID can resolve to either a global Racetify-user face (any account,
// any tenant) or a ref-based face belonging to tenantID - whichever it is,
// this method applies no further ownership check of its own: the RLS
// policy on faces (docs/face-tenant-enrollment-plan.md §2.3), already
// scoped to tenantID by the surrounding db.WithTenantTx, is what decides
// visibility. That is the entire mechanism behind "a tenant's staff/crew
// can search with any face_id" - there is nothing else to add here.
func (s *Service) Search(ctx context.Context, tenantID, eventID, faceID string, page pagination.PageParams) (pagination.Page[gallery.Photo], error) {
	var embeddings []Embedding
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if _, err := s.repo.GetFace(ctx, faceID); err != nil {
			return err
		}
		var err error
		embeddings, err = s.repo.ListEmbeddingsByFace(ctx, faceID)
		return err
	})
	if err != nil {
		return pagination.Page[gallery.Photo]{}, err
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
