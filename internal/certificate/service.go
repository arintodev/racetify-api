package certificate

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/fontlib"
	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
)

// Service implements the certificate use cases. Every mutating method takes
// the actor's role and re-checks it (defence in depth on top of the
// router-level gate), like the other bounded contexts.
type Service struct {
	db        *database.DB
	repo      *Repository
	audit     *audit.Repository
	store     objectstorage.Driver
	cfg       config.StorageConfig
	queue     *jobqueue.Queue
	templates *generator.Service
	storage   *storage.Service
	fonts     *fontlib.Service
}

func NewService(db *database.DB, repo *Repository, audit *audit.Repository, store objectstorage.Driver, cfg config.StorageConfig,
	queue *jobqueue.Queue, templates *generator.Service, storage *storage.Service, fonts *fontlib.Service) *Service {
	return &Service{db: db, repo: repo, audit: audit, store: store, cfg: cfg, queue: queue, templates: templates, storage: storage, fonts: fonts}
}

func (s *Service) recordAudit(ctx context.Context, tenantID, actorUserID, action string, metadata map[string]any) error {
	return s.audit.Record(ctx, &audit.Log{
		ID:          security.MustNewUUIDv4(),
		TenantID:    &tenantID,
		ActorUserID: &actorUserID,
		Action:      action,
		Metadata:    metadata,
		CreatedAt:   time.Now().UTC(),
	})
}

func requireAdmin(actor rbac.MemberRole) error {
	if !actor.IsAtLeast(rbac.RoleAdmin) {
		return domain.ErrForbidden
	}
	return nil
}

// FileURL returns a time-limited URL to fetch the certificate image; "" and
// no expiry when the record has no file.
func (s *Service) FileURL(c *Certificate) (url string, expires *time.Time, err error) {
	if c.StorageID == nil || c.ObjectKey == "" {
		return "", nil, nil
	}
	ticket, err := objectstorage.GetURL(s.store, objectstorage.Bucket(c.Bucket), c.TenantID, c.ObjectKey, s.cfg.DownloadTTL)
	if err != nil {
		return "", nil, err
	}
	if ticket.ExpiresAt.IsZero() {
		return ticket.URL, nil, nil
	}
	return ticket.URL, &ticket.ExpiresAt, nil
}

// Listing is an event's certificates and whether they are published.
type Listing struct {
	Items       []Certificate
	PublishedAt *time.Time
}

// List returns every certificate of the event (staff and up, via the router).
func (s *Service) List(ctx context.Context, tenantID, eventID string) (*Listing, error) {
	out := &Listing{}
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.EventExists(ctx, tenantID, eventID); err != nil {
			return err
		}
		var err error
		if out.Items, err = s.repo.List(ctx, tenantID, eventID); err != nil {
			return err
		}
		out.PublishedAt, err = s.repo.PublishedAt(ctx, tenantID, eventID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpsertInput is a save request for one participant's certificate.
type UpsertInput struct {
	// CertificateNo is used when the participant has none yet; an existing
	// number is kept.
	CertificateNo   string
	TemplateID      *string
	TemplateVersion *int
	DataHash        *string
	Status          string
	Error           *string
	StorageID       *string
	Format          *string
	DPI             *int
	WidthPx         *int
	HeightPx        *int
	SizeBytes       *int64
}

var numberPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{3,39}$`)

func (in *UpsertInput) validate() *Error {
	if !numberPattern.MatchString(in.CertificateNo) {
		return invalid("certificate_no", "certificate_no must be 4-40 letters, digits or dashes.")
	}
	if in.Status != StatusReady && in.Status != StatusFailed {
		return invalid("status", "status must be ready or failed.")
	}
	if in.Format != nil && *in.Format != "jpg" && *in.Format != "png" {
		return invalid("format", "format must be jpg or png.")
	}
	for field, v := range map[string]*int{"dpi": in.DPI, "width_px": in.WidthPx, "height_px": in.HeightPx} {
		if v != nil && (*v <= 0 || *v > 100000) {
			return invalid(field, field+" is out of range.")
		}
	}
	if in.SizeBytes != nil && *in.SizeBytes < 0 {
		return invalid("size_bytes", "size_bytes is out of range.")
	}
	if in.Status == StatusReady && (in.StorageID == nil || in.Format == nil) {
		return invalid("storage_id", "a ready certificate needs its file (storage_id and format).")
	}
	if in.Error != nil {
		msg := strings.TrimSpace(*in.Error)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		in.Error = &msg
	}
	return nil
}

func (s *Service) checkStorage(ctx context.Context, tenantID, storageID string) error {
	obj, err := s.repo.GetObject(ctx, tenantID, storageID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return invalid("storage_id", "storage_id is not an uploaded file of this workspace.")
		}
		return err
	}
	if obj.Status != "stored" {
		return invalid("storage_id", "the upload has not been completed yet.")
	}
	if obj.Bucket != "private" {
		return invalid("storage_id", "a certificate must be stored in the private bucket.")
	}
	if ct := strings.ToLower(obj.ContentType); ct != "image/jpeg" && ct != "image/png" {
		return invalid("storage_id", "a certificate must be a JPEG or PNG image.")
	}
	return nil
}

// Upsert saves the certificate of one participant (Admin+).
func (s *Service) Upsert(ctx context.Context, tenantID, eventID, participantID, actorUserID string, actorRole rbac.MemberRole, in UpsertInput) (*Certificate, error) {
	if err := requireAdmin(actorRole); err != nil {
		return nil, err
	}
	if verr := in.validate(); verr != nil {
		return nil, verr
	}

	var saved *Certificate
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.ParticipantInEvent(ctx, tenantID, eventID, participantID); err != nil {
			return err
		}
		if in.TemplateID != nil {
			ok, err := s.repo.TemplateInEvent(ctx, tenantID, eventID, *in.TemplateID)
			if err != nil {
				return err
			}
			if !ok {
				return invalid("template_id", "template_id is not a certificate template of this event.")
			}
		}
		if in.StorageID != nil {
			if err := s.checkStorage(ctx, tenantID, *in.StorageID); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		c := &Certificate{
			ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID, ParticipantID: participantID,
			CertificateNo: in.CertificateNo, TemplateID: in.TemplateID, TemplateVersion: in.TemplateVersion,
			DataHash: in.DataHash, Status: in.Status, Error: in.Error, StorageID: in.StorageID, Format: in.Format,
			DPI: in.DPI, WidthPx: in.WidthPx, HeightPx: in.HeightPx, SizeBytes: in.SizeBytes,
			CreatedBy: actorUserID, CreatedAt: now,
		}
		if in.Status == StatusReady {
			c.GeneratedAt = &now
		}
		if err := s.repo.Upsert(ctx, c); err != nil {
			return err
		}
		if in.Status == StatusReady {
			if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionCertificateGenerated,
				map[string]any{"event_id": eventID, "participant_id": participantID}); err != nil {
				return err
			}
		}
		var err error
		saved, err = s.repo.GetByParticipant(ctx, tenantID, eventID, participantID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// Delete removes a participant's certificate record (Admin+). The image stays
// in Object Storage as an unreferenced object.
func (s *Service) Delete(ctx context.Context, tenantID, eventID, participantID, actorUserID string, actorRole rbac.MemberRole) error {
	if err := requireAdmin(actorRole); err != nil {
		return err
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.Delete(ctx, tenantID, eventID, participantID); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionCertificateDeleted,
			map[string]any{"event_id": eventID, "participant_id": participantID})
	})
}

// SetPublished opens or closes the event's certificates to participants
// (Admin+) and returns the publish time (nil when closed).
func (s *Service) SetPublished(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, published bool) (*time.Time, error) {
	if err := requireAdmin(actorRole); err != nil {
		return nil, err
	}
	var at *time.Time
	if published {
		now := time.Now().UTC()
		at = &now
	}
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.EventExists(ctx, tenantID, eventID); err != nil {
			return err
		}
		if err := s.repo.SetPublished(ctx, tenantID, eventID, at); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionCertificatesPublished,
			map[string]any{"event_id": eventID, "published": published})
	})
	if err != nil {
		return nil, err
	}
	return at, nil
}
