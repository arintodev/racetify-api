package event

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/httpapi/middleware"
	"github.com/racetify/racetify-api/internal/mailer"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
)

// Service implements implementation_guide_phase_1.md §2's Event/Race step
// of the Organizer Journey ("EO mendaftarkan Tenant dan membuat Event
// beserta kategori Race"). Every mutating method takes actorRole and
// re-checks it (rbac.RoleAdmin+), mirroring internal/storage.Service's
// RequestUpload/CompleteUpload - defense in depth on top of the router-
// level mw.RequireAdminRole gate in routes.go, the same double-gating
// pattern used throughout this codebase for provisioning-type actions.
// Read methods take no actorRole: they rely solely on the router-level
// mw.RequireAnyRole gate, matching internal/tenant.Service.ListMembers.
// users *auth.Repository, mailer, and cfg exist solely for
// assignment_service.go's event_invitations flow (AssignToEvent/
// AcceptEventInvitation) - a one-directional, downward dependency on the
// auth bounded context, the exact same shape and justification as
// internal/tenant.Service's own "users *auth.Repository" field (see that
// struct's doc comment): looking up an inviter's display name, checking
// whether an invited email already has an account, and confirming an
// invitation acceptor's email matches. auth never imports event, so this
// is not a cycle.
type Service struct {
	db     *database.DB
	repo   *Repository
	audit  *audit.Repository
	users  *auth.Repository
	mailer mailer.Mailer
	cfg    config.AuthConfig
	// members backs RequireEventAccess's Path A re-verification (access.go)
	// - the exact same MembershipChecker shape mw.RequireTenantForUser
	// depends on, structurally satisfied by *tenant.Repository without
	// this package importing internal/tenant (see middleware.
	// MembershipChecker's own doc comment for why it lives there instead).
	members middleware.MembershipChecker
}

func NewService(
	db *database.DB,
	repo *Repository,
	audit *audit.Repository,
	users *auth.Repository,
	m mailer.Mailer,
	cfg config.AuthConfig,
	members middleware.MembershipChecker,
) *Service {
	return &Service{db: db, repo: repo, audit: audit, users: users, mailer: m, cfg: cfg, members: members}
}

// slugSanitizer/Slugify are duplicated from internal/tenant/service.go
// rather than shared: a single, trivial, dependency-free string helper is
// not worth a shared leaf package for two callers, and internal/event must
// not import internal/tenant - docs/phase1-api-plan.md §9's import
// direction has event depending only downward on internal/domain,
// internal/platform/*, and internal/httpapi/{routing,middleware,respond,
// reqctx}, never sideways on another Phase 0/Phase 1 bounded context.
var slugSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugSanitizer.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// ==================== events ====================

// CreateEvent creates a new Event, starting 'draft' - only a later
// SetEventStatus(published) call makes it resolve through the Runner
// Portal (docs/phase1-api-plan.md §5).
func (s *Service) CreateEvent(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, name, slug string, venue *string, startDate, endDate *time.Time) (*Event, error) {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return nil, domain.ErrForbidden
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("service: %w: name is required", domain.ErrInvalidState)
	}
	if slug == "" {
		slug = Slugify(name)
	} else {
		slug = Slugify(slug)
	}
	if slug == "" {
		return nil, fmt.Errorf("service: %w: event name/slug must contain at least one alphanumeric character", domain.ErrInvalidState)
	}

	now := time.Now().UTC()
	ev := &Event{
		ID:        security.MustNewUUIDv4(),
		TenantID:  tenantID,
		Name:      name,
		Slug:      slug,
		Venue:     venue,
		StartDate: startDate,
		EndDate:   endDate,
		Status:    EventStatusDraft,
		CreatedBy: actorUserID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.CreateEvent(ctx, ev); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionEventCreated, map[string]any{"event_id": ev.ID, "slug": slug})
	})
	if err != nil {
		return nil, err
	}
	return ev, nil
}

func (s *Service) GetEvent(ctx context.Context, tenantID, id string) (*Event, error) {
	var ev *Event
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		ev, err = s.repo.GetEventByID(ctx, tenantID, id)
		return err
	})
	return ev, err
}

// ListEvents returns one keyset-paginated page of tenantID's events - see
// internal/platform/pagination for why keyset rather than LIMIT/OFFSET.
func (s *Service) ListEvents(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[Event], error) {
	var out pagination.Page[Event]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListEvents(ctx, tenantID, page)
		return err
	})
	return out, err
}

// EventPatch carries only the fields a PATCH request actually supplied -
// nil means "leave unchanged". UpdateEvent merges this onto the current
// row before writing, the same convention every other bounded context's
// PATCH handler in this codebase follows.
type EventPatch struct {
	Name               *string
	Slug               *string
	Venue              *string
	StartDate          *time.Time
	EndDate            *time.Time
	LogoStorageID      *string
	ThumbnailStorageID *string
}

func (s *Service) UpdateEvent(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, id string, patch EventPatch) (*Event, error) {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return nil, domain.ErrForbidden
	}

	var ev *Event
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		current, err := s.repo.GetEventByID(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if patch.Name != nil {
			if strings.TrimSpace(*patch.Name) == "" {
				return fmt.Errorf("service: %w: name cannot be empty", domain.ErrInvalidState)
			}
			current.Name = *patch.Name
		}
		if patch.Slug != nil {
			slug := Slugify(*patch.Slug)
			if slug == "" {
				return fmt.Errorf("service: %w: slug must contain at least one alphanumeric character", domain.ErrInvalidState)
			}
			current.Slug = slug
		}
		if patch.Venue != nil {
			current.Venue = patch.Venue
		}
		if patch.StartDate != nil {
			current.StartDate = patch.StartDate
		}
		if patch.EndDate != nil {
			current.EndDate = patch.EndDate
		}
		if patch.LogoStorageID != nil {
			current.LogoStorageID = patch.LogoStorageID
		}
		if patch.ThumbnailStorageID != nil {
			current.ThumbnailStorageID = patch.ThumbnailStorageID
		}
		if err := s.repo.UpdateEvent(ctx, current); err != nil {
			return err
		}
		ev = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// SetEventStatus transitions an Event's status. Like
// internal/tenant.Service.SetTenantStatus, this deliberately does not
// enforce a strict draft->published->archived ordering (e.g. un-archiving
// back to draft is allowed) - Phase 1's P0 scope only needs "publishing
// makes the public routes resolve" to hold, not a full workflow state
// machine; add stricter transition rules later if real usage needs them.
func (s *Service) SetEventStatus(ctx context.Context, tenantID, actorUserID string, actorRole rbac.MemberRole, id string, status EventStatus) (*Event, error) {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return nil, domain.ErrForbidden
	}
	switch status {
	case EventStatusDraft, EventStatusPublished, EventStatusArchived:
	default:
		return nil, fmt.Errorf("service: %w: status must be one of draft, published, archived", domain.ErrInvalidState)
	}

	var ev *Event
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.UpdateEventStatus(ctx, tenantID, id, status); err != nil {
			return err
		}
		var err error
		ev, err = s.repo.GetEventByID(ctx, tenantID, id)
		if err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionEventStatusChanged, map[string]any{"event_id": id, "status": string(status)})
	})
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// ==================== races ====================

func (s *Service) CreateRace(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, name, slug string, distanceKM *float64) (*Race, error) {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return nil, domain.ErrForbidden
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("service: %w: name is required", domain.ErrInvalidState)
	}
	if slug == "" {
		slug = Slugify(name)
	} else {
		slug = Slugify(slug)
	}
	if slug == "" {
		return nil, fmt.Errorf("service: %w: race name/slug must contain at least one alphanumeric character", domain.ErrInvalidState)
	}

	now := time.Now().UTC()
	race := &Race{
		ID:         security.MustNewUUIDv4(),
		TenantID:   tenantID,
		EventID:    eventID,
		Name:       name,
		Slug:       slug,
		DistanceKM: distanceKM,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		// Confirm the parent event exists (and belongs to this tenant)
		// before inserting, so a bad event_id surfaces as domain.ErrNotFound
		// rather than an opaque FK-violation 500.
		if _, err := s.repo.GetEventByID(ctx, tenantID, eventID); err != nil {
			return err
		}
		if err := s.repo.CreateRace(ctx, race); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionRaceCreated, map[string]any{"event_id": eventID, "race_id": race.ID})
	})
	if err != nil {
		return nil, err
	}
	return race, nil
}

func (s *Service) GetRace(ctx context.Context, tenantID, eventID, id string) (*Race, error) {
	var race *Race
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		race, err = s.repo.GetRaceByID(ctx, tenantID, eventID, id)
		return err
	})
	return race, err
}

func (s *Service) ListRaces(ctx context.Context, tenantID, eventID string) ([]Race, error) {
	var out []Race
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListRaces(ctx, tenantID, eventID)
		return err
	})
	return out, err
}

// RacePatch carries only the fields a PATCH request actually supplied -
// nil means "leave unchanged", the same convention as EventPatch.
type RacePatch struct {
	Name       *string
	Slug       *string
	DistanceKM *float64
}

func (s *Service) UpdateRace(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, id string, patch RacePatch) (*Race, error) {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return nil, domain.ErrForbidden
	}

	var race *Race
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		current, err := s.repo.GetRaceByID(ctx, tenantID, eventID, id)
		if err != nil {
			return err
		}
		if patch.Name != nil {
			if strings.TrimSpace(*patch.Name) == "" {
				return fmt.Errorf("service: %w: name cannot be empty", domain.ErrInvalidState)
			}
			current.Name = *patch.Name
		}
		if patch.Slug != nil {
			slug := Slugify(*patch.Slug)
			if slug == "" {
				return fmt.Errorf("service: %w: slug must contain at least one alphanumeric character", domain.ErrInvalidState)
			}
			current.Slug = slug
		}
		if patch.DistanceKM != nil {
			current.DistanceKM = patch.DistanceKM
		}
		if err := s.repo.UpdateRace(ctx, current); err != nil {
			return err
		}
		race = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return race, nil
}

func (s *Service) DeleteRace(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, id string) error {
	if !actorRole.IsAtLeast(rbac.RoleAdmin) {
		return domain.ErrForbidden
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.repo.DeleteRace(ctx, tenantID, eventID, id)
	})
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
