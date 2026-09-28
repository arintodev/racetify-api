package face

import "time"

type FaceDTO struct {
	ID               string     `json:"id"`
	EventID          string     `json:"event_id"`
	UserID           string     `json:"user_id"`
	EmbeddingCount   int        `json:"embedding_count"`
	ConsentedAt      time.Time  `json:"consented_at"`
	ConsentRevokedAt *time.Time `json:"consent_revoked_at"`
	CreatedAt        time.Time  `json:"created_at"`
}

func faceResponse(f *Face) FaceDTO {
	return FaceDTO{
		ID: f.ID, EventID: f.EventID, UserID: f.UserID, EmbeddingCount: f.EmbeddingCount,
		ConsentedAt: f.ConsentedAt, ConsentRevokedAt: f.ConsentRevokedAt, CreatedAt: f.CreatedAt,
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

// ListEmbeddingsDTO answers GET .../users/{uid}/face-embeddings. Face is
// null when the user has not enrolled.
type ListEmbeddingsDTO struct {
	Face       *FaceDTO       `json:"face"`
	Embeddings []EmbeddingDTO `json:"embeddings"`
}

type searchRequest struct {
	FaceID string `json:"face_id"`
}
