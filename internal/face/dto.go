package face

import "time"

// FaceDTO's RefID is only ever non-nil for a tenant M2M-enrolled face -
// a Racetify user's own face has none (docs/face-tenant-enrollment-
// plan.md §2.1). Neither TenantID nor UserID/RefID's owning account is
// exposed here - a face_id is the only handle a caller needs.
type FaceDTO struct {
	ID             string    `json:"id"`
	RefID          *string   `json:"ref_id,omitempty"`
	EmbeddingCount int       `json:"embedding_count"`
	ConsentedAt    time.Time `json:"consented_at"`
	CreatedAt      time.Time `json:"created_at"`
}

func faceResponse(f *Face) FaceDTO {
	return FaceDTO{
		ID: f.ID, RefID: f.RefID, EmbeddingCount: f.EmbeddingCount,
		ConsentedAt: f.ConsentedAt, CreatedAt: f.CreatedAt,
	}
}

type EmbeddingDTO struct {
	ID              string    `json:"id"`
	FaceID          string    `json:"face_id"`
	ConfidenceScore *float64  `json:"confidence_score"`
	CreatedAt       time.Time `json:"created_at"`
}

func embeddingResponse(e *Embedding) EmbeddingDTO {
	return EmbeddingDTO{ID: e.ID, FaceID: e.FaceID, ConfidenceScore: e.ConfidenceScore, CreatedAt: e.CreatedAt}
}

// ListEmbeddingsDTO answers both the self and M2M "list" endpoints. Face is
// null when the subject has not enrolled.
type ListEmbeddingsDTO struct {
	Face       *FaceDTO       `json:"face"`
	Embeddings []EmbeddingDTO `json:"embeddings"`
}

func listEmbeddingsResponse(face *Face, embeddings []Embedding) ListEmbeddingsDTO {
	dto := ListEmbeddingsDTO{Embeddings: make([]EmbeddingDTO, len(embeddings))}
	if face != nil {
		f := faceResponse(face)
		dto.Face = &f
	}
	for i := range embeddings {
		dto.Embeddings[i] = embeddingResponse(&embeddings[i])
	}
	return dto
}

type searchRequest struct {
	FaceID string `json:"face_id"`
}
