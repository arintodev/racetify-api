package face

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// DetectedFace is one face the embed service found in an image: its
// bounding box (fractions of the image's width/height, matching
// photo_tags/photo_face_detections' convention), its embedding vector, and
// the detector's own confidence.
type DetectedFace struct {
	Box        Box       `json:"bbox"`
	Embedding  []float32 `json:"embedding"`
	Confidence float64   `json:"confidence"`
}

// Box mirrors gallery.Box: fractions of the photo's width/height.
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// EmbedClient calls the face-embedding microservice (face-embed-service/,
// deployed externally to Google Cloud Run - docs/face-search-plan.md, not
// part of this repo's docker-compose.yml). A generous timeout is used by
// default since a Cloud Run cold start after an idle period can take
// several seconds, unlike an in-network call.
type EmbedClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewEmbedClient constructs a client. baseURL is the Cloud Run service's
// HTTPS URL (cfg.FaceEmbed.ServiceURL); apiKey is sent as a shared-secret
// header (see face-embed-service's own auth doc, docs/face-search-plan.md's
// "Auth" section for the IAM-vs-shared-secret tradeoff this project made).
func NewEmbedClient(baseURL, apiKey string, timeout time.Duration) *EmbedClient {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &EmbedClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout},
	}
}

// Configured reports whether a service URL was set - callers (the
// detection job, enrollment) should fail closed with a clear error rather
// than attempt a request to an empty URL when it wasn't.
func (c *EmbedClient) Configured() bool { return c.baseURL != "" }

// DetectAndEmbed sends one image to POST /detect-embed and returns every
// face it found. An empty result (no error) means no face was detected -
// not a failure. One retry is attempted on a transient (connection-level)
// failure, matching this codebase's "known, accepted gap" resilience style
// elsewhere (see internal/jobqueue's Queue doc comment) rather than a full
// retry/backoff library for a single extra attempt.
func (c *EmbedClient) DetectAndEmbed(ctx context.Context, imageBytes []byte, filename string) ([]DetectedFace, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("face: FACE_EMBED_SERVICE_URL is not configured")
	}
	faces, err := c.detectAndEmbedOnce(ctx, imageBytes, filename)
	if err != nil {
		faces, err = c.detectAndEmbedOnce(ctx, imageBytes, filename)
	}
	if err != nil {
		return nil, fmt.Errorf("face: detect-embed request: %w", err)
	}
	return faces, nil
}

func (c *EmbedClient) detectAndEmbedOnce(ctx context.Context, imageBytes []byte, filename string) ([]DetectedFace, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("image", filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(imageBytes); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/detect-embed", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("X-Face-Embed-Key", c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("face-embed returned status %d: %s", resp.StatusCode, string(data))
	}
	var parsed struct {
		Faces []DetectedFace `json:"faces"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return parsed.Faces, nil
}
