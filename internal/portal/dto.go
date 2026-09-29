package portal

import (
	"github.com/racetify/racetify-api/internal/event"
	"github.com/racetify/racetify-api/internal/gallery"
)

// EventDTO is the public microsite header (docs/public-gallery-ux.md,
// e-certificate-ux.md Screen D): just enough to render the banner and
// theme the page, never anything from other tenant-scoped tables.
type EventDTO struct {
	Slug         string  `json:"slug"`
	Name         string  `json:"name"`
	Venue        *string `json:"venue,omitempty"`
	StartDate    *string `json:"start_date,omitempty"`
	EndDate      *string `json:"end_date,omitempty"`
	PrimaryColor *string `json:"primary_color,omitempty"`
}

func eventResponse(e *event.Event) EventDTO {
	dto := EventDTO{Slug: e.Slug, Name: e.Name, Venue: e.Venue, PrimaryColor: e.PrimaryColor}
	if e.StartDate != nil {
		d := e.StartDate.Format("2006-01-02")
		dto.StartDate = &d
	}
	if e.EndDate != nil {
		d := e.EndDate.Format("2006-01-02")
		dto.EndDate = &d
	}
	return dto
}

// CertificateMatchDTO is one search result - name + BIB only, the same
// "minimal match list ... no PII beyond what's needed to disambiguate"
// docs/phase1-api-plan.md §5 specifies.
type CertificateMatchDTO struct {
	ParticipantID string `json:"participant_id"`
	BibNumber     int    `json:"bib_number"`
	FullName      string `json:"full_name"`
	RaceName      string `json:"race_name"`
	Status        string `json:"status"`
}

func matchResponse(m CertificateMatch) CertificateMatchDTO {
	return CertificateMatchDTO{
		ParticipantID: m.ParticipantID, BibNumber: m.BibNumber,
		FullName: m.FullName, RaceName: m.RaceName, Status: m.Status,
	}
}

// searchResponse is GET .../certificates/search's body.
type searchResponse struct {
	Items []CertificateMatchDTO `json:"items"`
}

// downloadResponse is GET .../certificates/{pid}/download's body - a
// short-lived signed URL, not the image itself (matches how internal/
// certificate's own staff-facing CertificateDTO.FileURL works).
type downloadResponse struct {
	FileURL   string  `json:"file_url"`
	ExpiresAt *string `json:"file_url_expires_at,omitempty"`
}

// AlbumDTO is one public album (docs/public-gallery-ux.md's "Daftar
// Album") - no Description/CreatedBy, nothing a runner has no use for.
type AlbumDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PhotoCount int    `json:"photo_count"`
}

func albumResponse(a *gallery.Album) AlbumDTO {
	return AlbumDTO{ID: a.ID, Name: a.Name, PhotoCount: a.PhotoCount}
}

// PhotoDTO is one public photo: just the watermarked preview - never
// gallery.PhotoDTO's OriginalURL/UploadedBy/OCR fields (staff-gated
// dashboard only), and no BIB tags either - a runner searches by BIB, but
// a photo's tags are not displayed back publicly (they'd let anyone
// harvest which BIBs appear in which photos just by browsing an album).
type PhotoDTO struct {
	ID         string `json:"id"`
	AlbumID    string `json:"album_id"`
	AlbumName  string `json:"album_name"`
	PreviewURL string `json:"preview_url"`
}
