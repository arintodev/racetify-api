package generator

import (
	"context"
	"errors"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
)

// Service implements the template use cases. Every mutating method takes the
// actor's role and re-checks it (defence in depth on top of the router-level
// gate), like the other bounded contexts.
type Service struct {
	db    *database.DB
	repo  *Repository
	audit *audit.Repository
	store objectstorage.Driver
	cfg   config.StorageConfig
}

func NewService(db *database.DB, repo *Repository, audit *audit.Repository, store objectstorage.Driver, cfg config.StorageConfig) *Service {
	return &Service{db: db, repo: repo, audit: audit, store: store, cfg: cfg}
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

// SVGURL returns a way to fetch the template's SVG: a time-limited presigned
// URL for a private object (expiry set), a stable URL for a public one.
func (s *Service) SVGURL(t *Template) (url string, expires *time.Time, err error) {
	bucket := objectstorage.Bucket(t.Bucket)
	if bucket == objectstorage.BucketPublic {
		return s.store.PublicURL(t.TenantID, t.ObjectKey), nil, nil
	}
	ticket, err := s.store.PresignDownload(bucket, t.TenantID, t.ObjectKey, s.cfg.DownloadTTL)
	if err != nil {
		return "", nil, err
	}
	return ticket.URL, &ticket.ExpiresAt, nil
}

// checkScope validates the race a template is scoped to (nil: the whole event).
func (s *Service) checkScope(ctx context.Context, tenantID, eventID string, raceID *string) error {
	if raceID == nil {
		return nil
	}
	ok, err := s.repo.RaceInEvent(ctx, tenantID, eventID, *raceID)
	if err != nil {
		return err
	}
	if !ok {
		return invalid("race_id", "race_id is not a race of this event.")
	}
	return nil
}

func (s *Service) checkStorage(ctx context.Context, tenantID, storageID string) error {
	if storageID == "" {
		return invalid("storage_id", "storage_id is required.")
	}
	obj, err := s.repo.GetObject(ctx, tenantID, storageID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return invalid("storage_id", "storage_id is not an uploaded file of this workspace.")
		}
		return err
	}
	if cerr := checkObject(obj); cerr != nil {
		return cerr
	}
	return nil
}

// CreateInput is a create request.
type CreateInput struct {
	Kind      Kind
	Name      string
	RaceID    *string
	StorageID string
	// Active makes it the template picked for its scope; whichever template
	// held the scope until now becomes inactive.
	Active   bool
	Metadata []byte
}

func (s *Service) Create(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, in CreateInput) (*Template, error) {
	if err := requireAdmin(actorRole); err != nil {
		return nil, err
	}
	if !in.Kind.Valid() {
		return nil, invalid("service", "service must be bib or certificate.")
	}
	name, nerr := normalizeName(in.Name)
	if nerr != nil {
		return nil, nerr
	}
	metadata, merr := normalizeMetadata(in.Metadata)
	if merr != nil {
		return nil, merr
	}

	var created *Template
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.EventExists(ctx, tenantID, eventID); err != nil {
			return err
		}
		if err := s.checkScope(ctx, tenantID, eventID, in.RaceID); err != nil {
			return err
		}
		if err := s.checkStorage(ctx, tenantID, in.StorageID); err != nil {
			return err
		}
		now := time.Now().UTC()
		t := &Template{
			ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID, RaceID: in.RaceID,
			StorageID: in.StorageID, Name: name, Kind: in.Kind, IsActive: in.Active, Version: 1,
			Metadata: metadata, CreatedBy: actorUserID, CreatedAt: now, UpdatedAt: now,
		}
		if t.IsActive {
			if _, err := s.repo.Deactivate(ctx, tenantID, eventID, t.Kind, t.RaceID, t.ID); err != nil {
				return err
			}
		}
		if err := s.repo.Create(ctx, t); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionTemplateUploaded,
			map[string]any{"template_id": t.ID, "event_id": eventID, "service": string(t.Kind)}); err != nil {
			return err
		}
		var err error
		created, err = s.repo.Get(ctx, tenantID, eventID, t.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// List returns an event's templates; kind "" means every service.
func (s *Service) List(ctx context.Context, tenantID, eventID string, kind Kind) ([]Template, error) {
	if kind != "" && !kind.Valid() {
		return nil, invalid("service", "service must be bib or certificate.")
	}
	var out []Template
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.EventExists(ctx, tenantID, eventID); err != nil {
			return err
		}
		var err error
		out, err = s.repo.List(ctx, tenantID, eventID, kind)
		return err
	})
	return out, err
}

func (s *Service) Get(ctx context.Context, tenantID, eventID, id string) (*Template, error) {
	var t *Template
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		t, err = s.repo.Get(ctx, tenantID, eventID, id)
		return err
	})
	return t, err
}

// PatchInput is an update: a nil field is left as it is.
type PatchInput struct {
	Name *string
	// StorageID replaces the design (a new upload); the version goes up.
	StorageID *string
	// RaceID moves the template to a race; "" moves it to the whole event.
	RaceID *string
	// Active picks it (or stops picking it) for its scope.
	Active   *bool
	Metadata []byte
}

func (s *Service) Update(ctx context.Context, tenantID, eventID, id, actorUserID string, actorRole rbac.MemberRole, in PatchInput) (*Template, error) {
	if err := requireAdmin(actorRole); err != nil {
		return nil, err
	}
	var updated *Template
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		t, err := s.repo.Get(ctx, tenantID, eventID, id)
		if err != nil {
			return err
		}
		if in.Name != nil {
			name, nerr := normalizeName(*in.Name)
			if nerr != nil {
				return nerr
			}
			t.Name = name
		}
		if len(in.Metadata) > 0 {
			metadata, merr := normalizeMetadata(in.Metadata)
			if merr != nil {
				return merr
			}
			t.Metadata = metadata
		}
		if in.RaceID != nil {
			if *in.RaceID == "" {
				t.RaceID = nil
			} else {
				raceID := *in.RaceID
				t.RaceID = &raceID
			}
			if err := s.checkScope(ctx, tenantID, eventID, t.RaceID); err != nil {
				return err
			}
		}
		if in.Active != nil {
			t.IsActive = *in.Active
		}
		designChanged := in.StorageID != nil && *in.StorageID != t.StorageID
		if designChanged {
			if err := s.checkStorage(ctx, tenantID, *in.StorageID); err != nil {
				return err
			}
			t.StorageID = *in.StorageID
			t.Version++
		}
		// Taking a scope pushes its previous holder to inactive.
		if t.IsActive {
			if _, err := s.repo.Deactivate(ctx, tenantID, eventID, t.Kind, t.RaceID, t.ID); err != nil {
				return err
			}
		}
		if err := s.repo.Update(ctx, t); err != nil {
			return err
		}
		action := audit.ActionTemplateUpdated
		if designChanged {
			action = audit.ActionTemplateUploaded
		}
		if err := s.recordAudit(ctx, tenantID, actorUserID, action,
			map[string]any{"template_id": t.ID, "event_id": eventID, "version": t.Version}); err != nil {
			return err
		}
		updated, err = s.repo.Get(ctx, tenantID, eventID, t.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Service) Delete(ctx context.Context, tenantID, eventID, id, actorUserID string, actorRole rbac.MemberRole) error {
	if err := requireAdmin(actorRole); err != nil {
		return err
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.Delete(ctx, tenantID, eventID, id); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionTemplateDeleted,
			map[string]any{"template_id": id, "event_id": eventID})
	})
}
