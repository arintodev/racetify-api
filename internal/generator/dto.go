package generator

import (
	"encoding/json"
	"time"
)

// TemplateDTO is a template as the API returns it. The design itself is not
// inlined: SVGURL is where to fetch it (a presigned GET for the private
// bucket; SVGURLExpiresAt says until when).
type TemplateDTO struct {
	ID              string          `json:"id"`
	EventID         string          `json:"event_id"`
	RaceID          *string         `json:"race_id"`
	StorageID       string          `json:"storage_id"`
	Name            string          `json:"name"`
	Service         string          `json:"service"`
	IsActive        bool            `json:"is_active"`
	Version         int             `json:"version"`
	Metadata        json.RawMessage `json:"metadata"`
	SVGURL          string          `json:"svg_url"`
	SVGURLExpiresAt *time.Time      `json:"svg_url_expires_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

func templateResponse(t *Template, svgURL string, expires *time.Time) TemplateDTO {
	metadata := t.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	return TemplateDTO{
		ID: t.ID, EventID: t.EventID, RaceID: t.RaceID, StorageID: t.StorageID, Name: t.Name,
		Service: string(t.Kind), IsActive: t.IsActive, Version: t.Version, Metadata: metadata,
		SVGURL: svgURL, SVGURLExpiresAt: expires, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

type createRequest struct {
	Service   string          `json:"service"`
	Name      string          `json:"name"`
	RaceID    string          `json:"race_id"`
	StorageID string          `json:"storage_id"`
	IsActive  *bool           `json:"is_active"`
	Metadata  json.RawMessage `json:"metadata"`
}

// updateRequest: an absent field is left as it is; race_id "" moves the
// template to the whole event.
type updateRequest struct {
	Name      *string         `json:"name"`
	RaceID    *string         `json:"race_id"`
	StorageID *string         `json:"storage_id"`
	IsActive  *bool           `json:"is_active"`
	Metadata  json.RawMessage `json:"metadata"`
}
