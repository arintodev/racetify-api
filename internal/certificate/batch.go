package certificate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/templateassets"
)

// Making certificates in the background (docs/e-certificate-ux.md §5): a
// request lists the participants and, for each, the certificate number and
// the value of every tag the template uses (the dashboard already has the
// results in front of it, and its fingerprint of those values is what tells
// it a certificate is out of date). A worker draws the images with
// internal/generator, stores them, and writes the certificate records.

// MaxBatch is the most certificates one job makes.
const MaxBatch = 5000

const (
	maxTagsPerItem = 64
	maxValueLen    = 500
)

// BatchItem is one certificate to make.
type BatchItem struct {
	ParticipantID string            `json:"participant_id"`
	CertificateNo string            `json:"certificate_no"`
	DataHash      string            `json:"data_hash"`
	Values        map[string]string `json:"values"`
}

// BatchInput is a request to make certificates.
type BatchInput struct {
	TemplateID string
	Format     string // jpg | png
	DPI        int
	Items      []BatchItem
	ScopeLabel string
}

type batchPayload struct {
	EventID    string      `json:"event_id"`
	TemplateID string      `json:"template_id"`
	Format     string      `json:"format"`
	DPI        int         `json:"dpi"`
	Items      []BatchItem `json:"items"`
	Meta       batchMeta   `json:"meta"`
}

// batchMeta is shown in the job history (jobqueue passes it through); it
// holds no personal data.
type batchMeta struct {
	EventID      string `json:"event_id"`
	TemplateID   string `json:"template_id"`
	TemplateName string `json:"template_name"`
	ScopeLabel   string `json:"scope_label"`
	Format       string `json:"format"`
	DPI          int    `json:"dpi"`
	Count        int    `json:"count"`
}

func (in *BatchInput) validate() *Error {
	if in.Format != "jpg" && in.Format != "png" {
		return invalid("format", "format must be jpg or png.")
	}
	if in.DPI < 72 || in.DPI > 600 {
		return invalid("dpi", "dpi must be between 72 and 600.")
	}
	if n := len(in.Items); n == 0 || n > MaxBatch {
		return invalid("items", fmt.Sprintf("choose between 1 and %d participants.", MaxBatch))
	}
	seen := make(map[string]bool, len(in.Items))
	for i := range in.Items {
		it := &in.Items[i]
		if !numberPattern.MatchString(it.CertificateNo) {
			return invalid("items", "a certificate_no must be 4-40 letters, digits or dashes.")
		}
		if it.ParticipantID == "" || seen[it.ParticipantID] {
			return invalid("items", "each participant_id must be given once.")
		}
		seen[it.ParticipantID] = true
		if len(it.DataHash) > 64 {
			return invalid("items", "a data_hash is too long.")
		}
		if len(it.Values) > maxTagsPerItem {
			return invalid("items", "too many tag values for one certificate.")
		}
		for key, v := range it.Values {
			if len(v) > maxValueLen {
				return invalid("items", "the value of "+key+" is too long.")
			}
		}
	}
	return nil
}

func (s *Service) loadAssets(ctx context.Context, tenantID, eventID, templateID string) (*templateassets.Assets, error) {
	assets, err := templateassets.Load(ctx, s.templates, s.storage, s.fonts, tenantID, eventID, templateID, generator.KindCertificate)
	var problem *templateassets.Problem
	if errors.As(err, &problem) {
		return nil, invalid(problem.Field, problem.Message)
	}
	return assets, err
}

// StartBatch checks a request and queues its job (Admin+); it returns the job id.
func (s *Service) StartBatch(ctx context.Context, tenantID, eventID, actorUserID string, role rbac.MemberRole, in BatchInput) (string, error) {
	if err := requireAdmin(role); err != nil {
		return "", err
	}
	if verr := in.validate(); verr != nil {
		return "", verr
	}
	assets, err := s.loadAssets(ctx, tenantID, eventID, in.TemplateID)
	if err != nil {
		return "", err
	}
	// Proves the template can be drawn (layers and fonts) before queuing.
	if _, err := generator.NewImageRenderer(assets.Artwork, assets.Rasters, s.fonts.Provider(ctx)); err != nil {
		return "", invalid("template_id", err.Error())
	}

	label := strings.TrimSpace(in.ScopeLabel)
	if len(label) > 200 {
		label = label[:200]
	}
	raw, err := json.Marshal(batchPayload{
		EventID: eventID, TemplateID: in.TemplateID, Format: in.Format, DPI: in.DPI, Items: in.Items,
		Meta: batchMeta{
			EventID: eventID, TemplateID: in.TemplateID, TemplateName: assets.Template.Name, ScopeLabel: label,
			Format: in.Format, DPI: in.DPI, Count: len(in.Items),
		},
	})
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	return s.queue.Enqueue(ctx, tenantID, actorUserID, domain.JobTypeCertificatesBatch, payload)
}

// RunBatch is the certificates.batch handler: it makes each certificate,
// stores its image, and writes its record. One failing certificate is
// recorded as failed and does not stop the rest.
func (s *Service) RunBatch(ctx context.Context, job *jobqueue.Job) (map[string]any, string, error) {
	raw, err := json.Marshal(job.Payload)
	if err != nil {
		return nil, "", err
	}
	var p batchPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, "", fmt.Errorf("certificate: bad payload: %w", err)
	}
	assets, err := s.loadAssets(ctx, job.TenantID, p.EventID, p.TemplateID)
	if err != nil {
		return nil, "", err
	}
	renderer, err := generator.NewImageRenderer(assets.Artwork, assets.Rasters, s.fonts.Provider(ctx))
	if err != nil {
		return nil, "", err
	}

	total := len(p.Items)
	_ = s.queue.UpdateProgress(ctx, job, 0, total)
	lastReport := time.Now()
	failed := 0
	version := assets.Template.Version
	templateID := assets.Template.ID

	for i, item := range p.Items {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		in := UpsertInput{
			CertificateNo: item.CertificateNo, TemplateID: &templateID, TemplateVersion: &version,
			DataHash: strPtr(item.DataHash),
		}
		if err := s.makeOne(ctx, job, p, renderer, item, &in); err != nil {
			failed++
			msg := err.Error()
			in.Status, in.Error, in.StorageID = StatusFailed, &msg, nil
		} else {
			in.Status = StatusReady
		}
		if _, err := s.Upsert(ctx, job.TenantID, p.EventID, item.ParticipantID, job.CreatedBy, rbac.RoleAdmin, in); err != nil {
			// A record that cannot be written (the participant is gone, ...)
			// counts as failed; the job goes on.
			if in.Status == StatusReady {
				failed++
			}
		}
		if time.Since(lastReport) > time.Second || i == total-1 {
			lastReport = time.Now()
			_ = s.queue.UpdateProgress(ctx, job, i+1, total)
		}
	}

	result := map[string]any{"count": total, "failed": failed, "format": p.Format, "dpi": p.DPI, "template_version": version}
	if failed > 0 {
		return result, fmt.Sprintf("%d of %d certificates could not be made", failed, total), nil
	}
	return result, "", nil
}

// makeOne draws and stores one certificate's image, filling in the image's
// details on in.
func (s *Service) makeOne(ctx context.Context, job *jobqueue.Job, p batchPayload, renderer *generator.ImageRenderer, item BatchItem, in *UpsertInput) error {
	image, err := renderer.Render(generator.ImageInput{DPI: p.DPI, Format: p.Format, Values: item.Values})
	if err != nil {
		return err
	}
	contentType := "image/jpeg"
	if p.Format == "png" {
		contentType = "image/png"
	}
	key := fmt.Sprintf("certificates/%s/%s/%s.%s", p.EventID, item.ParticipantID, security.MustNewUUIDv4(), p.Format)
	obj, err := s.storage.StoreGenerated(ctx, job.TenantID, job.CreatedBy, string(objectstorage.BucketPrivate), key, contentType, image.Data)
	if err != nil {
		return err
	}
	dpi, w, h, size := p.DPI, image.WidthPx, image.HeightPx, obj.SizeBytes
	in.StorageID, in.Format, in.DPI, in.WidthPx, in.HeightPx, in.SizeBytes = &obj.ID, &p.Format, &dpi, &w, &h, &size
	return nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
