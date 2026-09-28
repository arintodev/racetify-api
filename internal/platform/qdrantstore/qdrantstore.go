// Package qdrantstore is a small control-plane wrapper around Qdrant's own
// REST API (https://api.qdrant.tech/), used to store and search the face
// embedding vectors described in docs/face-search-plan.md (racetify-app
// repo). It talks to Qdrant over plain HTTP/JSON via net/http, the same
// "no framework, minimal dependency footprint" style this codebase already
// follows elsewhere (see internal/platform/objectstorage's local driver) -
// Qdrant's official Go client pulls in a gRPC/protobuf dependency tree this
// package does not need for the handful of calls internal/face makes
// (Upsert, Search, Delete, EnsureCollection).
//
// internal/face never stores a Qdrant client's raw response shape - it only
// ever sees this package's own Point/SearchResult types - so a future
// switch to the official client, or to a different vector store entirely,
// would not leak past this package.
package qdrantstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config is New's input. CollectionName is the single collection this
// wrapper reads and writes (docs/face-search-plan.md's "one collection:
// face_points").
type Config struct {
	Addr           string // host:port, e.g. "localhost:6333" or "qdrant:6333"
	APIKey         string // blank in local dev, see .env.example
	CollectionName string
}

// Store is the qdrantstore.qdrantstore control-plane client. It holds no
// vector-shaped knowledge itself beyond what Upsert/Search/Delete need -
// internal/face owns what tenant_id/event_id/face_id/etc mean.
type Store struct {
	baseURL    string
	apiKey     string
	collection string
	client     *http.Client
}

// New constructs a Store. It does not call Qdrant yet - EnsureCollection
// does that, and is meant to be called once at boot (internal/app.Build),
// the same "fail fast at startup" shape objectstorage.NewDriver uses.
func New(cfg Config) *Store {
	base := cfg.Addr
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	return &Store{
		baseURL:    strings.TrimRight(base, "/"),
		apiKey:     cfg.APIKey,
		collection: cfg.CollectionName,
		// Cloud Run-hosted dependencies elsewhere in this feature (the
		// face-embed service) need a generous timeout for cold starts;
		// Qdrant itself is expected to run in the same docker-compose
		// network, so a short timeout is appropriate here - a hung Qdrant
		// call should not stall a background job indefinitely.
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Point is one vector plus its filterable payload fields - the shape both
// Upsert and Search deal in. Payload fields are pointers/omitted-when-empty
// so a detection point (no FaceID/UserID) and an enrollment point (no
// PhotoID) each only carry what applies to them, matching
// docs/face-search-plan.md's Qdrant payload section.
type Point struct {
	ID      string
	Vector  []float32
	Payload Payload
}

// Payload is a point's filterable metadata. TenantID/EventID/Source are
// always set; PhotoID is set for source="detection" points, FaceID/UserID
// for source="enrollment" points - see docs/face-search-plan.md's Qdrant
// section for why detection points never carry FaceID/UserID (there is no
// clustering step that would assign them).
type Payload struct {
	TenantID string `json:"tenant_id"`
	EventID  string `json:"event_id"`
	Source   string `json:"source"` // "detection" | "enrollment"
	PhotoID  string `json:"photo_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	FaceID   string `json:"face_id,omitempty"`
}

// SearchResult is one match from Search: a point id, its payload (so the
// caller can read photo_id/face_id back off it) and the similarity score.
type SearchResult struct {
	ID      string
	Score   float64
	Payload Payload
}

// Filter narrows Search/Delete to points matching every non-empty field
// (an AND of "must" conditions, in Qdrant's terms). Empty fields are not
// included in the filter.
type Filter struct {
	TenantID string
	EventID  string
	Source   string
}

// EnsureCollection creates the collection if it does not already exist
// (idempotent - safe to call on every boot). vectorSize is the embedding
// model's output dimension (512 for ArcFace/InsightFace, per
// docs/face-search-plan.md).
func (s *Store) EnsureCollection(ctx context.Context, vectorSize int) error {
	exists, err := s.collectionExists(ctx)
	if err != nil {
		return fmt.Errorf("qdrantstore: check collection: %w", err)
	}
	if exists {
		return nil
	}
	body := map[string]any{
		"vectors": map[string]any{
			"size":     vectorSize,
			"distance": "Cosine",
		},
	}
	_, err = s.do(ctx, http.MethodPut, "/collections/"+s.collection, body)
	if err != nil {
		return fmt.Errorf("qdrantstore: create collection: %w", err)
	}
	return nil
}

func (s *Store) collectionExists(ctx context.Context) (bool, error) {
	resp, err := s.doRaw(ctx, http.MethodGet, "/collections/"+s.collection, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("qdrantstore: unexpected status %d", resp.StatusCode)
	}
	return true, nil
}

// Upsert writes one point (insert or overwrite by id). Called once per
// detected face / enrolled embedding, with a wait=true request so the point
// is searchable immediately after this call returns - the background job
// and enrollment endpoint both need read-your-writes here, not eventual
// consistency.
func (s *Store) Upsert(ctx context.Context, p Point) error {
	body := map[string]any{
		"points": []map[string]any{
			{"id": p.ID, "vector": p.Vector, "payload": payloadMap(p.Payload)},
		},
	}
	_, err := s.do(ctx, http.MethodPut, "/collections/"+s.collection+"/points?wait=true", body)
	if err != nil {
		return fmt.Errorf("qdrantstore: upsert point %s: %w", p.ID, err)
	}
	return nil
}

// Search returns up to limit nearest neighbours of vector, restricted to
// points matching filter, ordered by descending similarity.
func (s *Store) Search(ctx context.Context, vector []float32, filter Filter, limit int) ([]SearchResult, error) {
	body := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
		"filter":       filterMap(filter),
	}
	resp, err := s.do(ctx, http.MethodPost, "/collections/"+s.collection+"/points/search", body)
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: search: %w", err)
	}
	var parsed struct {
		Result []struct {
			ID      string  `json:"id"`
			Score   float64 `json:"score"`
			Payload Payload `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, fmt.Errorf("qdrantstore: decode search response: %w", err)
	}
	out := make([]SearchResult, len(parsed.Result))
	for i, r := range parsed.Result {
		out[i] = SearchResult{ID: r.ID, Score: r.Score, Payload: r.Payload}
	}
	return out, nil
}

// GetVector fetches a single point's own vector by id (with_vector=true).
// Used by face search to turn an enrolled embedding's stored point id back
// into a query vector - see internal/face.Service.pointVector.
func (s *Store) GetVector(ctx context.Context, pointID string) ([]float32, error) {
	resp, err := s.do(ctx, http.MethodGet, "/collections/"+s.collection+"/points/"+pointID+"?with_vector=true", nil)
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: get point %s: %w", pointID, err)
	}
	var parsed struct {
		Result struct {
			Vector []float32 `json:"vector"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, fmt.Errorf("qdrantstore: decode point response: %w", err)
	}
	return parsed.Result.Vector, nil
}

// Delete removes the given point ids. Used wherever a Postgres row that
// owns a qdrant_point_id is deleted (a photo, a revoked face's embeddings)
// - Postgres ON DELETE CASCADE never reaches Qdrant, so every such deletion
// path must call this explicitly (docs/face-search-plan.md's Privacy
// section).
func (s *Store) Delete(ctx context.Context, pointIDs []string) error {
	if len(pointIDs) == 0 {
		return nil
	}
	body := map[string]any{"points": pointIDs}
	_, err := s.do(ctx, http.MethodPost, "/collections/"+s.collection+"/points/delete?wait=true", body)
	if err != nil {
		return fmt.Errorf("qdrantstore: delete points: %w", err)
	}
	return nil
}

func payloadMap(p Payload) map[string]any {
	m := map[string]any{"tenant_id": p.TenantID, "event_id": p.EventID, "source": p.Source}
	if p.PhotoID != "" {
		m["photo_id"] = p.PhotoID
	}
	if p.UserID != "" {
		m["user_id"] = p.UserID
	}
	if p.FaceID != "" {
		m["face_id"] = p.FaceID
	}
	return m
}

func filterMap(f Filter) map[string]any {
	var must []map[string]any
	add := func(key, value string) {
		if value != "" {
			must = append(must, map[string]any{"key": key, "match": map[string]any{"value": value}})
		}
	}
	add("tenant_id", f.TenantID)
	add("event_id", f.EventID)
	add("source", f.Source)
	if len(must) == 0 {
		return nil
	}
	return map[string]any{"must": must}
}

// do performs a request and returns the raw response body, treating any
// non-2xx status as an error (with Qdrant's own error body included, where
// present, for diagnosability).
func (s *Store) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	resp, err := s.doRaw(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("qdrant returned status %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

func (s *Store) doRaw(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.apiKey != "" {
		req.Header.Set("api-key", s.apiKey)
	}
	return s.client.Do(req)
}
