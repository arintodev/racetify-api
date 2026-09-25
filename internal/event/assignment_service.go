package event

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/invitepreview"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
)

// normalizeEmail is duplicated from internal/tenant/service.go rather than
// shared - see that file's own doc comment for why: a single, trivial,
// dependency-free string helper is not worth a shared leaf package for a
// handful of callers.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validateAssignmentInput(label AssignmentLabel, capabilities []string) error {
	if !label.Valid() {
		return fmt.Errorf("service: %w: label must be one of crew, volunteer, photographer, timing", domain.ErrInvalidState)
	}
	if len(capabilities) == 0 {
		return fmt.Errorf("service: %w: at least one capability is required", domain.ErrInvalidState)
	}
	for _, c := range capabilities {
		if !IsValidCapability(c) {
			return fmt.Errorf("service: %w: unknown capability %q", domain.ErrInvalidState, c)
		}
	}
	return nil
}

// AssignToEvent implements the tenant-staff side of assigning event-scoped
// crew/volunteer access (docs/event-crew-access-plan.md §3): if email
// already has a Racetify account, the grant activates immediately;
// otherwise a pending EventInvitation is issued (mirroring internal/
// tenant.Service.InviteStaff's shape) and the grant only exists once
// accepted. actorRole only needs to be Staff (not Admin) - assigning crew
// to one's own event is "operasional event tertentu", not a tenant-wide
// administrative action (phase1-api-plan.md §4's own Staff-gate rationale).
//
// Exactly one of the three non-error return values is populated: an
// *EventAssignment (existing-account path) or an *EventInvitation plus its
// raw token (new-account path).
func (s *Service) AssignToEvent(
	ctx context.Context,
	tenantID, eventID, actorUserID string,
	actorRole rbac.MemberRole,
	email string,
	label AssignmentLabel,
	capabilities []string,
	assignmentExpiresAt *time.Time,
	linkOrigin string,
) (*EventAssignment, *EventInvitation, string, error) {
	if !actorRole.IsAtLeast(rbac.RoleStaff) {
		return nil, nil, "", domain.ErrForbidden
	}
	if err := validateAssignmentInput(label, capabilities); err != nil {
		return nil, nil, "", err
	}
	email = normalizeEmail(email)

	existingUser, err := s.users.GetUserByEmail(ctx, email)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, nil, "", err
	}

	now := time.Now().UTC()

	if existingUser != nil {
		a := &EventAssignment{
			ID:           security.MustNewUUIDv4(),
			TenantID:     tenantID,
			EventID:      eventID,
			UserID:       existingUser.ID,
			Label:        label,
			Capabilities: capabilities,
			Status:       AssignmentStatusActive,
			AssignedBy:   actorUserID,
			ExpiresAt:    assignmentExpiresAt,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
			// Confirm the event exists in this tenant before inserting, so
			// a bad event_id surfaces as domain.ErrNotFound rather than an
			// opaque FK-violation 500 - same reasoning as
			// Service.CreateRace.
			if _, err := s.repo.GetEventByID(ctx, tenantID, eventID); err != nil {
				return err
			}
			if err := s.repo.CreateAssignment(ctx, a); err != nil {
				return err
			}
			return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionEventAssignmentCreated,
				map[string]any{"event_id": eventID, "user_id": existingUser.ID, "label": string(label)})
		})
		if err != nil {
			return nil, nil, "", err
		}
		return a, nil, "", nil
	}

	rawToken, err := security.GenerateOpaqueToken(24)
	if err != nil {
		return nil, nil, "", err
	}
	inv := &EventInvitation{
		ID:                  security.MustNewUUIDv4(),
		TenantID:            tenantID,
		EventID:             eventID,
		Email:               email,
		Label:               label,
		Capabilities:        capabilities,
		TokenHash:           security.HashToken(rawToken),
		InvitedBy:           actorUserID,
		Status:              EventInvitationStatusPending,
		ExpiresAt:           now.Add(s.cfg.InvitationTokenTTL),
		AssignmentExpiresAt: assignmentExpiresAt,
		CreatedAt:           now,
	}

	var ev *Event
	var inviter *auth.User
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		ev, err = s.repo.GetEventByID(ctx, tenantID, eventID)
		if err != nil {
			return err
		}
		if err := s.repo.CreateEventInvitation(ctx, inv); err != nil {
			return err
		}
		inviter, err = s.users.GetUserByID(ctx, actorUserID)
		if err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionEventAssignmentInvited,
			map[string]any{"event_id": eventID, "email": email, "label": string(label)})
	})
	if err != nil {
		return nil, nil, "", err
	}

	_ = s.mailer.SendEventInvitationEmail(ctx, email, ev.Name, inviter.DisplayName(), eventInvitationLink(linkOrigin, rawToken))
	return nil, inv, rawToken, nil
}

// ListAssignments returns one keyset-paginated page of an event's active
// crew/volunteer assignments.
func (s *Service) ListAssignments(ctx context.Context, tenantID, eventID string, page pagination.PageParams) (pagination.Page[EventAssignment], error) {
	var out pagination.Page[EventAssignment]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListAssignments(ctx, tenantID, eventID, page)
		return err
	})
	return out, err
}

// AssignmentPatch carries only the fields a PATCH request actually
// supplied - nil means "leave unchanged", same convention as EventPatch.
type AssignmentPatch struct {
	Label        *AssignmentLabel
	Capabilities []string // nil means "leave unchanged"; a non-nil empty slice is rejected below
	ExpiresAt    *time.Time
	ClearExpiry  bool // true clears ExpiresAt to NULL even though ExpiresAt above is nil
}

func (s *Service) UpdateAssignment(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, id string, patch AssignmentPatch) (*EventAssignment, error) {
	if !actorRole.IsAtLeast(rbac.RoleStaff) {
		return nil, domain.ErrForbidden
	}

	var a *EventAssignment
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		current, err := s.repo.GetAssignmentByID(ctx, tenantID, eventID, id)
		if err != nil {
			return err
		}
		if patch.Label != nil {
			current.Label = *patch.Label
		}
		if patch.Capabilities != nil {
			current.Capabilities = patch.Capabilities
		}
		if err := validateAssignmentInput(current.Label, current.Capabilities); err != nil {
			return err
		}
		if patch.ClearExpiry {
			current.ExpiresAt = nil
		} else if patch.ExpiresAt != nil {
			current.ExpiresAt = patch.ExpiresAt
		}
		if err := s.repo.UpdateAssignment(ctx, current); err != nil {
			return err
		}
		a = current
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionEventAssignmentUpdated, map[string]any{"assignment_id": id})
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) RevokeAssignment(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, id string) error {
	if !actorRole.IsAtLeast(rbac.RoleStaff) {
		return domain.ErrForbidden
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.repo.RevokeAssignment(ctx, tenantID, eventID, id); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionEventAssignmentRevoked, map[string]any{"assignment_id": id})
	})
}

// AcceptEventInvitation implements the invitee's side of the event-scoped
// invite flow (docs/event-crew-access-plan.md §3) - mirrors internal/
// tenant.Service.AcceptInvitation's shape exactly: acceptingUserID must
// belong to an already-authenticated user whose account email matches the
// invitation (they log in or register first, then redeem the link), the
// same Single Identity guarantee tenant staff onboarding relies on.
func (s *Service) AcceptEventInvitation(ctx context.Context, rawToken, acceptingUserID string) (*EventAssignment, error) {
	inv, err := s.repo.GetEventInvitationByTokenHash(ctx, security.HashToken(rawToken))
	if err != nil {
		return nil, err
	}
	if inv.Status != EventInvitationStatusPending {
		return nil, domain.ErrInvalidState
	}
	if inv.IsExpired(time.Now().UTC()) {
		return nil, domain.ErrTokenExpired
	}

	user, err := s.users.GetUserByID(ctx, acceptingUserID)
	if err != nil {
		return nil, err
	}
	if normalizeEmail(user.Email) != normalizeEmail(inv.Email) {
		return nil, fmt.Errorf("service: %w: invitation was issued to a different email address", domain.ErrForbidden)
	}

	var a *EventAssignment
	err = s.db.WithTenantTx(ctx, inv.TenantID, func(ctx context.Context) error {
		if err := s.repo.MarkEventInvitationAccepted(ctx, inv.ID, time.Now().UTC()); err != nil {
			return err
		}
		now := time.Now().UTC()
		a = &EventAssignment{
			ID:           security.MustNewUUIDv4(),
			TenantID:     inv.TenantID,
			EventID:      inv.EventID,
			UserID:       acceptingUserID,
			Label:        inv.Label,
			Capabilities: inv.Capabilities,
			Status:       AssignmentStatusActive,
			AssignedBy:   inv.InvitedBy,
			ExpiresAt:    inv.AssignmentExpiresAt,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := s.repo.CreateAssignment(ctx, a); err != nil {
			return err
		}
		return s.recordAudit(ctx, inv.TenantID, acceptingUserID, audit.ActionEventAssignmentAccepted, map[string]any{"invitation_id": inv.ID})
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// MyAssignments answers "every event I'm assigned to" for the caller's own
// side (docs/event-crew-access-ux.md §3.1's landing picker) - see
// Repository.MyAssignments' doc comment for why this is safe to answer
// without an existing tenant context.
func (s *Service) MyAssignments(ctx context.Context, userID string) ([]EventAssignmentWithEvent, error) {
	return s.repo.MyAssignments(ctx, userID)
}

func eventInvitationLink(origin, token string) string {
	return strings.TrimRight(origin, "/") + "/invite/" + token
}

// PreviewInvitation implements invitepreview.Source for event (crew)
// invitations.
func (s *Service) PreviewInvitation(ctx context.Context, rawToken string) (*invitepreview.Preview, error) {
	inv, err := s.repo.GetEventInvitationByTokenHash(ctx, security.HashToken(rawToken))
	if err != nil {
		return nil, err
	}
	var ev *Event
	err = s.db.WithTenantTx(ctx, inv.TenantID, func(ctx context.Context) error {
		var err error
		ev, err = s.repo.GetEventByID(ctx, inv.TenantID, inv.EventID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &invitepreview.Preview{
		Kind:         invitepreview.KindEvent,
		Name:         ev.Name,
		EventSlug:    ev.Slug,
		Label:        string(inv.Label),
		Capabilities: inv.Capabilities,
		Email:        inv.Email,
		Status:       invitepreview.ResolveStatus(string(inv.Status), inv.ExpiresAt, time.Now().UTC()),
		ExpiresAt:    inv.ExpiresAt,
	}, nil
}
