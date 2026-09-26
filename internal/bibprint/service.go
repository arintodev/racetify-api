// Package bibprint makes the print-ready BIB files (docs/bib-pdf-gopdf-plan.md):
// a request becomes a background job (generator.bib_batch) that draws the
// participants' BIBs on sheets with internal/generator's renderer and stores
// the PDF (or a ZIP of PDFs) in Object Storage for download.
//
// What is static in a template (the design, images) is drawn by the browser
// into PNG layers when the template is saved, and listed in the template's
// metadata as print.layers (object ids); text and QR codes are drawn here as
// vectors, per participant.
package bibprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/fontlib"
	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/participant"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
	"github.com/racetify/racetify-api/internal/templateassets"
)

// MaxPeople is the most BIBs one job prints.
const MaxPeople = 20000

// Service starts BIB print jobs and runs them.
type Service struct {
	db           *database.DB
	queue        *jobqueue.Queue
	audit        *audit.Repository
	templates    *generator.Service
	participants *participant.Service
	storage      *storage.Service
	fonts        *fontlib.Service
}

func NewService(db *database.DB, queue *jobqueue.Queue, audit *audit.Repository, templates *generator.Service, participants *participant.Service, storage *storage.Service, fonts *fontlib.Service) *Service {
	return &Service{db: db, queue: queue, audit: audit, templates: templates, participants: participants, storage: storage, fonts: fonts}
}

// Error is a failure the client can act on.
type Error struct {
	Status  int
	Code    string
	Field   string
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(field, message string) *Error {
	return &Error{Status: 400, Code: "invalid_request", Field: field, Message: message}
}

// StartInput is a print request.
type StartInput struct {
	TemplateID string
	Layout     generator.Layout
	// ParticipantIDs are the BIBs to print; the job puts them in print order.
	ParticipantIDs []string
	// ScopeLabel describes the selection for the history ("Relay 4×10K · Terdaftar").
	ScopeLabel string
}

// payload is what a job carries: ids and settings, no personal data.
type payload struct {
	EventID        string           `json:"event_id"`
	TemplateID     string           `json:"template_id"`
	Layout         generator.Layout `json:"layout"`
	ParticipantIDs []string         `json:"participant_ids"`
	// Meta is shown in the job history (jobqueue passes it through).
	Meta meta `json:"meta"`
}

type meta struct {
	EventID      string           `json:"event_id"`
	TemplateID   string           `json:"template_id"`
	TemplateName string           `json:"template_name"`
	ScopeLabel   string           `json:"scope_label"`
	Count        int              `json:"count"`
	Layout       generator.Layout `json:"layout"`
}

func requireAdmin(role rbac.MemberRole) error {
	if !role.IsAtLeast(rbac.RoleAdmin) {
		return domain.ErrForbidden
	}
	return nil
}

func (s *Service) loadAssets(ctx context.Context, tenantID, eventID, templateID string) (*templateassets.Assets, error) {
	assets, err := templateassets.Load(ctx, s.templates, s.storage, s.fonts, tenantID, eventID, templateID, generator.KindBIB)
	var problem *templateassets.Problem
	if errors.As(err, &problem) {
		return nil, invalid(problem.Field, problem.Message)
	}
	return assets, err
}

// Start checks a print request and queues its job (Admin+). It returns the job id.
func (s *Service) Start(ctx context.Context, tenantID, eventID, actorUserID string, role rbac.MemberRole, in StartInput) (string, error) {
	if err := requireAdmin(role); err != nil {
		return "", err
	}
	if n := len(in.ParticipantIDs); n == 0 || n > MaxPeople {
		return "", invalid("participant_ids", fmt.Sprintf("choose between 1 and %d participants.", MaxPeople))
	}
	assets, err := s.loadAssets(ctx, tenantID, eventID, in.TemplateID)
	if err != nil {
		return "", err
	}
	cell := generator.Size{W: assets.Artwork.WidthMM, H: assets.Artwork.HeightMM}
	if err := in.Layout.Validate(cell); err != nil {
		return "", invalid("layout", err.Error())
	}

	label := strings.TrimSpace(in.ScopeLabel)
	if len(label) > 200 {
		label = label[:200]
	}
	p := payload{
		EventID: eventID, TemplateID: in.TemplateID, Layout: in.Layout, ParticipantIDs: in.ParticipantIDs,
		Meta: meta{EventID: eventID, TemplateID: in.TemplateID, TemplateName: assets.Template.Name, ScopeLabel: label, Count: len(in.ParticipantIDs), Layout: in.Layout},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return "", err
	}
	jobID, err := s.queue.Enqueue(ctx, tenantID, actorUserID, domain.JobTypeGeneratorBibBatch, asMap)
	if err != nil {
		return "", err
	}
	_ = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.audit.Record(ctx, &audit.Log{
			ID: security.MustNewUUIDv4(), TenantID: &tenantID, ActorUserID: &actorUserID,
			Action:    audit.ActionBibGenerationRequested,
			Metadata:  map[string]any{"event_id": eventID, "job_id": jobID, "count": len(in.ParticipantIDs)},
			CreatedAt: time.Now().UTC(),
		})
	})
	return jobID, nil
}

func tagContext(row participant.PrintRow, eventName string) generator.TagContext {
	p := row.Participant
	ctx := generator.TagContext{
		Participant: generator.Participant{
			BibNumber: p.BibNumber, BibName: p.BibName, FirstName: p.FirstName, LastName: p.LastName,
			Gender: string(p.Gender), BloodType: p.BloodType, Club: p.Club, LegOrder: p.LegOrder,
			EmergencyContactName: p.EmergencyContactName, EmergencyContactPhone: p.EmergencyContactPhone,
		},
		EventName: eventName,
	}
	if row.RaceName != "" {
		ctx.Race = &generator.RaceInfo{Name: row.RaceName}
	}
	if p.TeamID != nil && row.TeamName != "" {
		ctx.Team = &generator.TeamInfo{Name: row.TeamName, GenderCategory: string(row.TeamCategory)}
	}
	return ctx
}

// Run is the generator.bib_batch handler: it draws the sheets, stores the
// file, and reports where it is in the result.
func (s *Service) Run(ctx context.Context, job *jobqueue.Job) (map[string]any, string, error) {
	raw, err := json.Marshal(job.Payload)
	if err != nil {
		return nil, "", err
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, "", fmt.Errorf("bibprint: bad payload: %w", err)
	}

	assets, err := s.loadAssets(ctx, job.TenantID, p.EventID, p.TemplateID)
	if err != nil {
		return nil, "", err
	}
	eventName, err := s.eventName(ctx, job.TenantID, p.EventID)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.participants.PrintRows(ctx, job.TenantID, p.EventID, p.ParticipantIDs)
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", errors.New("none of the chosen participants exist any more")
	}

	// Print order: team members together, by BIB.
	people := make([]generator.Person, len(rows))
	byID := make(map[string]participant.PrintRow, len(rows))
	for i, row := range rows {
		p := row.Participant
		people[i] = generator.Person{ID: p.ID, BibNumber: p.BibNumber, TeamID: p.TeamID, LegOrder: p.LegOrder}
		byID[p.ID] = row
	}
	ordered := generator.PrintOrder(people)
	contexts := make([]generator.TagContext, len(ordered))
	for i, person := range ordered {
		contexts[i] = tagContext(byID[person.ID], eventName)
	}

	total := len(contexts)
	_ = s.queue.UpdateProgress(ctx, job, 0, total)
	lastReport := time.Now()
	res, err := generator.Render(ctx, generator.RenderInput{
		Artwork: assets.Artwork, Rasters: assets.Rasters, Layout: p.Layout, People: contexts, Fonts: s.fonts.Provider(ctx),
	}, func(done int) {
		if time.Since(lastReport) > time.Second || done == total {
			lastReport = time.Now()
			_ = s.queue.UpdateProgress(ctx, job, done, total)
		}
	})
	if err != nil {
		return nil, "", err
	}

	base := "BIB_" + safeName(assets.Template.Name) + "_" + time.Now().UTC().Format("20060102-1504")
	name, contentType, data, err := generator.Bundle(res.Files, base)
	if err != nil {
		return nil, "", err
	}
	key := fmt.Sprintf("generated/bib/%s/%s/%s", p.EventID, job.ID, name)
	obj, err := s.storage.StoreGenerated(ctx, job.TenantID, job.CreatedBy, string(objectstorage.BucketPrivate), key, contentType, data)
	if err != nil {
		return nil, "", err
	}

	result := map[string]any{
		"object_id": obj.ID, "bucket": string(obj.Bucket), "key": key, "file_name": name,
		"content_type": contentType, "size_bytes": obj.SizeBytes, "files": len(res.Files),
		"sheets": res.Sheets, "count": total, "template_version": assets.Template.Version,
	}
	if missing := len(p.ParticipantIDs) - len(rows); missing > 0 {
		return result, fmt.Sprintf("%d participants were skipped (no longer in the event)", missing), nil
	}
	return result, "", nil
}

func (s *Service) eventName(ctx context.Context, tenantID, eventID string) (string, error) {
	var name string
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.db.Q(ctx).QueryRowContext(ctx,
			`SELECT name FROM events WHERE id = $1 AND tenant_id = $2`, eventID, tenantID).Scan(&name)
	})
	return name, err
}

// Download is where a finished print can be fetched.
type Download struct {
	URL       string
	ExpiresAt *time.Time
	FileName  string
}

// Download returns a time-limited link to a finished job's file (Admin+).
func (s *Service) Download(ctx context.Context, tenantID, eventID, jobID string, role rbac.MemberRole) (*Download, error) {
	if err := requireAdmin(role); err != nil {
		return nil, err
	}
	job, err := s.queue.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	if job.Type != domain.JobTypeGeneratorBibBatch || payloadEvent(job) != eventID {
		return nil, domain.ErrNotFound
	}
	if job.Status != domain.JobStatusCompleted && job.Status != domain.JobStatusCompletedWithErrors {
		return nil, fmt.Errorf("bibprint: %w: the print is not finished", domain.ErrInvalidState)
	}
	key, _ := job.Result["key"].(string)
	bucket, _ := job.Result["bucket"].(string)
	fileName, _ := job.Result["file_name"].(string)
	if key == "" || bucket == "" {
		return nil, domain.ErrNotFound
	}
	ticket, err := s.storage.RequestDownload(ctx, tenantID, key, bucket)
	if err != nil {
		return nil, err
	}
	d := &Download{URL: ticket.URL, FileName: fileName}
	if !ticket.ExpiresAt.IsZero() {
		d.ExpiresAt = &ticket.ExpiresAt
	}
	return d, nil
}

func payloadEvent(job *jobqueue.Job) string {
	s, _ := job.Payload["event_id"].(string)
	return s
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "template"
	}
	return out
}
