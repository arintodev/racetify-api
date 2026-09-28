package watermark

import (
	"context"
	"errors"
	"fmt"
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
)

// Service implements the watermark use cases. Every mutating method takes
// the actor's role and re-checks it (defence in depth on top of the
// router-level gate), like the other bounded contexts.
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

func requireRole(actor, min rbac.MemberRole) error {
	if !actor.IsAtLeast(min) {
		return domain.ErrForbidden
	}
	return nil
}

// PreviewURL returns where to fetch a watermark's source image: a stable
// public URL, or a time-limited presigned one for a private object -
// mirrors generator.Service.SVGURL / gallery.Service.PhotoURLs.
func (s *Service) PreviewURL(w *Watermark) (url string, expires *time.Time, err error) {
	ticket, err := objectstorage.GetURL(s.store, objectstorage.Bucket(w.Bucket), w.TenantID, w.ObjectKey, s.cfg.DownloadTTL)
	if err != nil {
		return "", nil, err
	}
	if ticket.ExpiresAt.IsZero() {
		return ticket.URL, nil, nil
	}
	return ticket.URL, &ticket.ExpiresAt, nil
}

// checkWatermarkObject decides whether an uploaded object can be a
// watermark's source image: a finished upload of an image file (mirrors
// generator's checkObject, relaxed to any image/* content type rather than
// SVG-only, and without a bucket restriction - a watermark's source image
// may live in either bucket, unlike a template's SVG).
func checkWatermarkObject(o *ObjectRef) *Error {
	if o.Status != "stored" {
		return invalid("storage_id", "the upload has not been completed yet.")
	}
	if !strings.HasPrefix(strings.ToLower(o.ContentType), "image/") {
		return invalid("storage_id", "a watermark must be an image file.")
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
	if cerr := checkWatermarkObject(obj); cerr != nil {
		return cerr
	}
	return nil
}

// List returns an event's watermark layers, in display order. Read access
// is Staff+ (the same floor gallery/generator reads use).
func (s *Service) List(ctx context.Context, tenantID, eventID string, actorRole rbac.MemberRole) ([]*Watermark, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return nil, err
	}
	var out []*Watermark
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.EventExists(ctx, tenantID, eventID); err != nil {
			return err
		}
		var err error
		out, err = s.repo.ListByEvent(ctx, tenantID, eventID)
		return err
	})
	return out, err
}

// ReplaceItem is one layer of a PUT /watermarks request. ID is "" for a
// newly added layer (Service.ReplaceAll assigns it a fresh UUID); a
// non-empty ID reuses an existing layer's identity.
type ReplaceItem struct {
	ID             string
	StorageID      string
	Name           string
	AnchorX        string
	AnchorY        string
	OffsetXPercent float64
	OffsetYPercent float64
	WidthPercent   float64
	AspectRatio    float64
	Opacity        float64
	SortOrder      int
}

func validateItem(in ReplaceItem) *Error {
	if !validAnchorX[in.AnchorX] {
		return invalid("anchor_x", "anchor_x must be left, center or right.")
	}
	if !validAnchorY[in.AnchorY] {
		return invalid("anchor_y", "anchor_y must be top, center or bottom.")
	}
	if in.Opacity < 0 || in.Opacity > 1 {
		return invalid("opacity", "opacity must be between 0 and 1.")
	}
	if in.WidthPercent <= 0 {
		return invalid("width_percent", "width_percent must be positive.")
	}
	if in.AspectRatio <= 0 {
		return invalid("aspect_ratio", "aspect_ratio must be positive.")
	}
	return nil
}

func validateName(name string) (string, *Error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", invalid("name", "name is required.")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", invalid("name", "name is too long.")
	}
	return name, nil
}

// ReplaceAll validates and replaces an event's entire set of watermark
// layers in one tenant-scoped transaction (delete-all-for-event then bulk
// insert), and writes one audit entry. Admin-only, re-checked here like
// every other mutating method in this codebase (defence in depth on top of
// the router-level gate).
func (s *Service) ReplaceAll(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, items []ReplaceItem) ([]*Watermark, error) {
	if err := requireRole(actorRole, rbac.RoleAdmin); err != nil {
		return nil, err
	}
	if len(items) > MaxWatermarks {
		return nil, invalid("watermarks", fmt.Sprintf("at most %d watermarks per event.", MaxWatermarks))
	}

	var out []*Watermark
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.EventExists(ctx, tenantID, eventID); err != nil {
			return err
		}
		now := time.Now().UTC()
		built := make([]*Watermark, len(items))
		for i, in := range items {
			if verr := validateItem(in); verr != nil {
				return verr
			}
			name, nerr := validateName(in.Name)
			if nerr != nil {
				return nerr
			}
			if err := s.checkStorage(ctx, tenantID, in.StorageID); err != nil {
				return err
			}
			id := in.ID
			if id == "" {
				id = security.MustNewUUIDv4()
			}
			built[i] = &Watermark{
				ID: id, TenantID: tenantID, EventID: eventID, StorageID: in.StorageID, Name: name,
				AnchorX: in.AnchorX, AnchorY: in.AnchorY,
				OffsetXPercent: in.OffsetXPercent, OffsetYPercent: in.OffsetYPercent,
				WidthPercent: in.WidthPercent, AspectRatio: in.AspectRatio, Opacity: in.Opacity,
				SortOrder: in.SortOrder, CreatedAt: now, UpdatedAt: now,
			}
		}
		if err := s.repo.ReplaceAll(ctx, tenantID, eventID, built); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionWatermarksReplaced,
			map[string]any{"event_id": eventID, "watermarks": len(built)}); err != nil {
			return err
		}
		var err error
		out, err = s.repo.ListByEvent(ctx, tenantID, eventID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
