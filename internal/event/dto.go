package event

import "time"

// dateOnlyLayout is how StartDate/EndDate round-trip through JSON -
// events.start_date/end_date are Postgres DATE columns (migrations/
// 0006_events_races.up.sql), so the wire format is a plain "2006-01-02"
// calendar date, not a full RFC3339 timestamp.
const dateOnlyLayout = "2006-01-02"

type EventDTO struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Slug               string  `json:"slug"`
	Venue              *string `json:"venue,omitempty"`
	StartDate          *string `json:"start_date,omitempty"`
	EndDate            *string `json:"end_date,omitempty"`
	LogoStorageID      *string `json:"logo_storage_id,omitempty"`
	ThumbnailStorageID *string `json:"thumbnail_storage_id,omitempty"`
	Status             string  `json:"status"`
	CreatedAt          string  `json:"created_at"`
}

func eventResponse(e *Event) EventDTO {
	return EventDTO{
		ID:                 e.ID,
		Name:               e.Name,
		Slug:               e.Slug,
		Venue:              e.Venue,
		StartDate:          formatDateOnly(e.StartDate),
		EndDate:            formatDateOnly(e.EndDate),
		LogoStorageID:      e.LogoStorageID,
		ThumbnailStorageID: e.ThumbnailStorageID,
		Status:             string(e.Status),
		CreatedAt:          e.CreatedAt.Format(time.RFC3339),
	}
}

func formatDateOnly(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(dateOnlyLayout)
	return &s
}

// parseDateOnly is the DTO-decode counterpart to formatDateOnly. A nil or
// empty input means "not supplied" (distinguished by the caller checking
// the pointer before calling this, not by this function).
func parseDateOnly(s string) (*time.Time, error) {
	t, err := time.Parse(dateOnlyLayout, s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

type RaceDTO struct {
	ID         string   `json:"id"`
	EventID    string   `json:"event_id"`
	Name       string   `json:"name"`
	Slug       string   `json:"slug"`
	DistanceKM *float64 `json:"distance_km,omitempty"`
	CreatedAt  string   `json:"created_at"`
}

func raceResponse(r *Race) RaceDTO {
	return RaceDTO{
		ID:         r.ID,
		EventID:    r.EventID,
		Name:       r.Name,
		Slug:       r.Slug,
		DistanceKM: r.DistanceKM,
		CreatedAt:  r.CreatedAt.Format(time.RFC3339),
	}
}
