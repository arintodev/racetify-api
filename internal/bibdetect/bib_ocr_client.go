package bibdetect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DetectedBIB is one BIB text reading the photo bib service found in an
// image. Unlike internal/face's DetectedFace, there is no bounding box in
// this service's response - a photo_tags row made from this carries a NULL
// box (the schema already allows it, same as a manual tag).
type DetectedBib struct {
	Text       string
	Confidence float64
}

type BibOCRClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewBibOCRClient constructs a client. baseURL is the service's HTTPS URL
// (config.BibOCRConfig.ServiceURL); apiKey, if set, is sent as a
// shared-secret header (the service's own reference client sends none, so
// this is optional - the header is simply omitted when apiKey is "").
func NewBibOCRClient(baseURL, apiKey string, timeout time.Duration) *BibOCRClient {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &BibOCRClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout},
	}
}

// Configured reports whether a service URL was set - callers (the photo
// processing job) should skip the OCR step gracefully rather than attempt
// a request to an empty URL when it wasn't (docs/media-gallery-
// integration.md: thumbnailing must not depend on this service's
// availability).
func (c *BibOCRClient) Configured() bool { return c.baseURL != "" }

// processURLRequest/processURLResponse mirror the photo bib service's own
// contract exactly (POST {baseURL}/process-url, body {"url": "..."},
// answering {"success": bool, "bib_results": [{"texts": [{"text",
// "confidence"}]}]}) - one bib_result can carry several candidate text
// readings for the same detected region; ProcessURL flattens all of them,
// across every bib_result, into one list.
type processURLRequest struct {
	URL string `json:"url"`
}

type processURLResponse struct {
	Success    bool `json:"success"`
	BibResults []struct {
		Texts []struct {
			Text       string  `json:"text"`
			Confidence float64 `json:"confidence"`
		} `json:"texts"`
	} `json:"bib_results"`
}

// ProcessURL asks the photo bib service to read every BIB in the photo at
// photoURL (a downloadable URL - internal/gallery.Service.runOCR builds one
// via objectstorage.GetURL, since this service fetches the image itself
// rather than accepting an upload). An empty result (no error) means no BIB
// was found - not a failure. One retry is attempted on a transient
// (connection-level) failure, matching internal/face.EmbedClient.
// DetectAndEmbed's resilience style.
func (c *BibOCRClient) ProcessURL(ctx context.Context, photoURL string) ([]DetectedBib, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("gallery: BIB_OCR_SERVICE_URL is not configured")
	}
	tags, err := c.processURLOnce(ctx, photoURL)
	if err != nil {
		tags, err = c.processURLOnce(ctx, photoURL)
	}
	if err != nil {
		return nil, fmt.Errorf("gallery: process-url request: %w", err)
	}
	return tags, nil
}

func (c *BibOCRClient) processURLOnce(ctx context.Context, photoURL string) ([]DetectedBib, error) {
	body, err := json.Marshal(processURLRequest{URL: photoURL})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/process-url", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-Photo-Bib-Key", c.apiKey)
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
		return nil, fmt.Errorf("photo bib service responded with status %d: %s", resp.StatusCode, string(data))
	}
	var parsed processURLResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if !parsed.Success {
		return nil, fmt.Errorf("photo bib service reported failure")
	}

	var out []DetectedBib
	for _, r := range parsed.BibResults {
		for _, t := range r.Texts {
			out = append(out, DetectedBib{Text: t.Text, Confidence: t.Confidence})
		}
	}
	return out, nil
}
