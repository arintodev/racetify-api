package gallery

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
)

// Service implements the gallery use cases. Every mutating method takes the
// actor's role and re-checks it (defence in depth on top of the router-level
// gate), like the other bounded contexts.
type Service struct {
	db      *database.DB
	repo    *Repository
	audit   *audit.Repository
	storage *storage.Service
	store   objectstorage.Driver
	cfg     config.StorageConfig
}

func NewService(db *database.DB, repo *Repository, audit *audit.Repository, objects *storage.Service, store objectstorage.Driver, cfg config.StorageConfig) *Service {
	return &Service{db: db, repo: repo, audit: audit, storage: objects, store: store, cfg: cfg}
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

func requireRole(actor, min rbac.MemberRole) error {
	if !actor.IsAtLeast(min) {
		return domain.ErrForbidden
	}
	return nil
}

func validateAlbumName(name string) (string, *Error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", invalid("name", "name is required.")
	}
	if utf8.RuneCountInString(name) > maxAlbumNameLen {
		return "", invalid("name", "name is too long.")
	}
	return name, nil
}

// ==================== albums ====================

// AlbumInput is a create request. A new album is always a draft; publishing
// is a separate, deliberate update.
type AlbumInput struct {
	Name        string
	Description *string
}

func (s *Service) CreateAlbum(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, in AlbumInput) (*Album, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return nil, err
	}
	name, verr := validateAlbumName(in.Name)
	if verr != nil {
		return nil, verr
	}
	now := time.Now().UTC()
	a := &Album{
		ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID,
		Name: name, Description: in.Description, IsPublic: false,
		CreatedBy: actorUserID, CreatedAt: now, UpdatedAt: now,
	}
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.CreateAlbum(ctx, a); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionAlbumCreated,
			map[string]any{"album_id": a.ID, "event_id": eventID, "name": a.Name})
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) ListAlbums(ctx context.Context, tenantID, eventID string) ([]Album, error) {
	var out []Album
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListAlbums(ctx, tenantID, eventID)
		return err
	})
	return out, err
}

func (s *Service) GetAlbum(ctx context.Context, tenantID, eventID, id string) (*Album, error) {
	if !isUUID(id) {
		return nil, domain.ErrNotFound
	}
	var a *Album
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		a, err = s.repo.GetAlbum(ctx, tenantID, eventID, id)
		return err
	})
	return a, err
}

// AlbumPatch carries only the fields an update supplied; a nil pointer
// leaves the field unchanged. An empty description clears it.
type AlbumPatch struct {
	Name        *string
	Description *string
	IsPublic    *bool
}

func (s *Service) UpdateAlbum(ctx context.Context, tenantID, eventID, id, actorUserID string, actorRole rbac.MemberRole, p AlbumPatch) (*Album, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return nil, err
	}
	if !isUUID(id) {
		return nil, domain.ErrNotFound
	}
	var updated *Album
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		a, err := s.repo.GetAlbum(ctx, tenantID, eventID, id)
		if err != nil {
			return err
		}
		changed := map[string]any{"album_id": a.ID, "event_id": eventID}
		if p.Name != nil {
			name, verr := validateAlbumName(*p.Name)
			if verr != nil {
				return verr
			}
			a.Name = name
			changed["name"] = name
		}
		if p.Description != nil {
			if d := strings.TrimSpace(*p.Description); d == "" {
				a.Description = nil
			} else {
				a.Description = &d
			}
		}
		if p.IsPublic != nil && *p.IsPublic != a.IsPublic {
			a.IsPublic = *p.IsPublic
			changed["is_public"] = a.IsPublic
		}
		if err := s.repo.UpdateAlbum(ctx, a); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionAlbumUpdated, changed); err != nil {
			return err
		}
		updated = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteAlbum hard-deletes the album and its photos (Phase 1).
func (s *Service) DeleteAlbum(ctx context.Context, tenantID, eventID, id, actorUserID string, actorRole rbac.MemberRole) error {
	if err := requireRole(actorRole, rbac.RoleAdmin); err != nil {
		return err
	}
	if !isUUID(id) {
		return domain.ErrNotFound
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		a, err := s.repo.GetAlbum(ctx, tenantID, eventID, id)
		if err != nil {
			return err
		}
		if err := s.repo.DeleteAlbum(ctx, tenantID, eventID, id); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionAlbumDeleted,
			map[string]any{"album_id": id, "event_id": eventID, "name": a.Name, "photos": a.PhotoCount})
	})
}
