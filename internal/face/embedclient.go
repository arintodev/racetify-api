package face

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Box is a bounding box as fractions of an image's width/height (matching
// photo_face_detections.box_x/y/w/h and photo_tags' own convention). The
// face-detector service itself reports pixel coordinates - see
// DetectedFace's doc comment and boxFraction.
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// DetectedFace is one face the face-detector service found in an image:
// its raw pixel bounding box [x1,y1,x2,y2] (InsightFace's own convention -
// face-detector/app/face_engine.py's face.bbox), its embedding vector, and
// the detector's own confidence (det_score). BBoxPixel is only meaningful
// to a caller that also knows the source image's pixel dimensions
// (detection_job.go's detectPhoto, via boxFraction) - enrollment
// (service.go's detectBestFace) never uses it, since an enrolled embedding
// is not tied to any photo location.
type DetectedFace struct {
	BBoxPixel  [4]float64
	Embedding  []float32
	Confidence float64
}

// EmbedClient calls the face-detector microservice (the face-detector/
// repo, deployed externally - not part of this repo's docker-compose.yml).
// It is URL-based: the service fetches the image itself rather than
// accepting an upload, so every call needs a downloadable URL, not raw
// bytes - see service.go's tempUploadOwn/tempUploadByRef and detection_job.
// go's detectPhoto for how each caller gets one. A generous default
// timeout is used since a cold start (model load, or the service's own
// image download) can take several seconds.
type EmbedClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewEmbedClient constructs a client. baseURL is the face-detector
// service's HTTPS URL (config.FaceEmbedConfig.ServiceURL); apiKey, if set,
// is sent as a shared-secret header (the service's own reference
// implementation checks none, so this is optional).
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

// Configured reports whether a service URL was set - callers (enrollment,
// the detection job) should fail closed with a clear error rather than
// attempt a request to an empty URL when it wasn't.
func (c *EmbedClient) Configured() bool { return c.baseURL != "" }

// detectSingleRequest/photoResultDTO/faceResultDTO mirror the
// face-detector service's own schema exactly (face-detector/app/
// schemas.py's BatchPhotoItem/PhotoResult/FaceResult): POST
// {baseURL}/detect/single, body {"photo_id", "url"}, answering
// {"status": "success"|"error", "error", "faces": [{"bbox", "det_score",
// "embedding", ...}]}. landmarks/age/gender are part of that service's
// response too but unused here - nothing in this codebase stores them.
type detectSingleRequest struct {
	PhotoID string `json:"photo_id"`
	URL     string `json:"url"`
}

type faceResultDTO struct {
	Bbox      [4]float64 `json:"bbox"`
	DetScore  float64    `json:"det_score"`
	Embedding []float32  `json:"embedding"`
}

type photoResultDTO struct {
	Status string          `json:"status"`
	Error  string          `json:"error"`
	Faces  []faceResultDTO `json:"faces"`
}

// DetectAndEmbed sends {photo_id, url} to POST /detect/single and returns
// every face the service found. photoID is an opaque label the service
// just echoes back - it never needs to be a real gallery photo id;
// detectBestFace (enrollment) passes a throwaway one. An empty result (no
// error) means no face was detected - not a failure. A PhotoResult.status
// of "error" is translated into a Go error carrying its message. One retry
// is attempted on a transient (connection-level) failure, matching
// gallery.OCRClient.ProcessURL's resilience style.
func (c *EmbedClient) DetectAndEmbed(ctx context.Context, photoID, url string) ([]DetectedFace, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("face: FACE_EMBED_SERVICE_URL is not configured")
	}
	faces, err := c.detectOnce(ctx, photoID, url)
	if err != nil {
		faces, err = c.detectOnce(ctx, photoID, url)
	}
	if err != nil {
		return nil, fmt.Errorf("face: detect/single request: %w", err)
	}
	return faces, nil
}

func (c *EmbedClient) detectOnce(ctx context.Context, photoID, url string) ([]DetectedFace, error) {
	body, err := json.Marshal(detectSingleRequest{PhotoID: photoID, URL: url})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/detect/single", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return nil, fmt.Errorf("face-detector returned status %d: %s", resp.StatusCode, string(data))
	}
	var parsed photoResultDTO
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if parsed.Status == "error" {
		return nil, fmt.Errorf("face-detector: %s", parsed.Error)
	}

	out := make([]DetectedFace, len(parsed.Faces))
	for i, f := range parsed.Faces {
		out[i] = DetectedFace{BBoxPixel: f.Bbox, Embedding: f.Embedding, Confidence: f.DetScore}
	}
	return out, nil
}

// boxFraction converts a face-detector pixel bbox [x1,y1,x2,y2] into the
// fraction-of-image-width/height Box every stored box in this codebase
// uses. width/height must be the exact pixel dimensions of the image at
// the URL that was analyzed - detectPhoto passes photo.Width/Height, the
// stored original's own dimensions, matching what the face-detector
// downloaded.
func boxFraction(bbox [4]float64, width, height int) Box {
	if width <= 0 || height <= 0 {
		return Box{}
	}
	w, h := float64(width), float64(height)
	return Box{
		X: bbox[0] / w,
		Y: bbox[1] / h,
		W: (bbox[2] - bbox[0]) / w,
		H: (bbox[3] - bbox[1]) / h,
	}
}
