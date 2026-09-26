package certificate

import "time"

// CertificateDTO is a certificate as the API returns it. The image is not
// inlined: FileURL is where to fetch it (a presigned GET for the private
// bucket; FileURLExpiresAt says until when), empty when there is no file.
type CertificateDTO struct {
	ParticipantID    string     `json:"participant_id"`
	CertificateNo    string     `json:"certificate_no"`
	TemplateID       *string    `json:"template_id"`
	TemplateVersion  *int       `json:"template_version"`
	DataHash         *string    `json:"data_hash"`
	Status           string     `json:"status"`
	Error            *string    `json:"error"`
	StorageID        *string    `json:"storage_id"`
	Format           *string    `json:"format"`
	DPI              *int       `json:"dpi"`
	WidthPx          *int       `json:"width_px"`
	HeightPx         *int       `json:"height_px"`
	SizeBytes        *int64     `json:"size_bytes"`
	GeneratedAt      *time.Time `json:"generated_at"`
	FileURL          string     `json:"file_url"`
	FileURLExpiresAt *time.Time `json:"file_url_expires_at,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ListDTO is an event's certificates and when they were published (null: not).
type ListDTO struct {
	Items       []CertificateDTO `json:"items"`
	PublishedAt *time.Time       `json:"published_at"`
}

func certificateResponse(c *Certificate, fileURL string, expires *time.Time) CertificateDTO {
	return CertificateDTO{
		ParticipantID: c.ParticipantID, CertificateNo: c.CertificateNo, TemplateID: c.TemplateID,
		TemplateVersion: c.TemplateVersion, DataHash: c.DataHash, Status: c.Status, Error: c.Error,
		StorageID: c.StorageID, Format: c.Format, DPI: c.DPI, WidthPx: c.WidthPx, HeightPx: c.HeightPx,
		SizeBytes: c.SizeBytes, GeneratedAt: c.GeneratedAt, FileURL: fileURL, FileURLExpiresAt: expires,
		UpdatedAt: c.UpdatedAt,
	}
}

// upsertRequest: template_id "" means no template.
type upsertRequest struct {
	CertificateNo   string  `json:"certificate_no"`
	TemplateID      string  `json:"template_id"`
	TemplateVersion *int    `json:"template_version"`
	DataHash        *string `json:"data_hash"`
	Status          string  `json:"status"`
	Error           *string `json:"error"`
	StorageID       string  `json:"storage_id"`
	Format          *string `json:"format"`
	DPI             *int    `json:"dpi"`
	WidthPx         *int    `json:"width_px"`
	HeightPx        *int    `json:"height_px"`
	SizeBytes       *int64  `json:"size_bytes"`
}

func (r upsertRequest) input() UpsertInput {
	in := UpsertInput{
		CertificateNo: r.CertificateNo, TemplateVersion: r.TemplateVersion, DataHash: r.DataHash,
		Status: r.Status, Error: r.Error, Format: r.Format, DPI: r.DPI, WidthPx: r.WidthPx,
		HeightPx: r.HeightPx, SizeBytes: r.SizeBytes,
	}
	if r.TemplateID != "" {
		in.TemplateID = &r.TemplateID
	}
	if r.StorageID != "" {
		in.StorageID = &r.StorageID
	}
	return in
}
