package tenant

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/invitepreview"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/security"
)

// MemberWithUser is a membership plus the person it belongs to - what the
// "Tim & Akses" screen shows (a bare user_id means nothing to a reader).
type MemberWithUser struct {
	TenantMember
	Email     string
	FirstName string
	LastName  string
}

// authorizeMemberChange is the rule set for changing or removing another
// member, kept as a pure function so it is testable without a database.
// newRole is nil for a removal.
//
//   - Only Owner/Admin may manage members at all.
//   - The Owner is untouchable here: there is exactly one, and handing the
//     workspace over is an ownership transfer, not a role change.
//   - Only the Owner may change roles (grant or revoke Admin).
//   - An Admin may remove Staff, never another Admin; the Owner may remove
//     Admin or Staff.
func authorizeMemberChange(actorRole MemberRole, target *TenantMember, newRole *MemberRole) error {
	if !actorRole.IsAtLeast(RoleAdmin) {
		return domain.ErrForbidden
	}
	if target.Status != MemberStatusActive {
		return domain.ErrNotFound
	}
	if target.Role == RoleOwner {
		return domain.ErrForbidden
	}
	if newRole != nil {
		if *newRole != RoleAdmin && *newRole != RoleStaff {
			return fmt.Errorf("service: %w: role must be admin or staff", domain.ErrInvalidState)
		}
		if actorRole != RoleOwner {
			return domain.ErrForbidden
		}
		return nil
	}
	if actorRole == RoleAdmin && target.Role != RoleStaff {
		return domain.ErrForbidden
	}
	return nil
}

// ListMembers returns one keyset-paginated page of tenantID's active
// members, with their names and emails.
func (s *Service) ListMembers(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[MemberWithUser], error) {
	var out pagination.Page[MemberWithUser]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListActiveMembersWithUser(ctx, tenantID, page)
		return err
	})
	return out, err
}

// UpdateMemberRole changes a member's role (see authorizeMemberChange).
func (s *Service) UpdateMemberRole(ctx context.Context, tenantID, actorUserID string, actorRole MemberRole, memberID string, newRole MemberRole) (*TenantMember, error) {
	var member *TenantMember
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		target, err := s.repo.GetMemberByID(ctx, tenantID, memberID)
		if err != nil {
			return err
		}
		if err := authorizeMemberChange(actorRole, target, &newRole); err != nil {
			return err
		}
		if err := s.repo.UpdateMemberRole(ctx, tenantID, memberID, newRole); err != nil {
			return err
		}
		target.Role = newRole
		member = target
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionMemberRoleChanged,
			map[string]any{"member_id": memberID, "user_id": target.UserID, "role": string(newRole)})
	})
	if err != nil {
		return nil, err
	}
	return member, nil
}

// RemoveMember ends a member's access. The row is kept with status
// 'removed' (audit history, and accepting a fresh invitation later
// reactivates it); access stops at once because every request re-checks
// membership (middleware.RequireTenantForUser).
func (s *Service) RemoveMember(ctx context.Context, tenantID, actorUserID string, actorRole MemberRole, memberID string) error {
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		target, err := s.repo.GetMemberByID(ctx, tenantID, memberID)
		if err != nil {
			return err
		}
		if err := authorizeMemberChange(actorRole, target, nil); err != nil {
			return err
		}
		if err := s.repo.SetMemberStatus(ctx, tenantID, memberID, MemberStatusRemoved); err != nil {
			return err
		}
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionMemberRemoved,
			map[string]any{"member_id": memberID, "user_id": target.UserID})
	})
}

// ResendInvitation replaces a pending invitation's token (the old link stops
// working), extends its expiry, and emails the new link. linkOrigin is the
// already-validated frontend origin the link should point at.
func (s *Service) ResendInvitation(ctx context.Context, tenantID, actorUserID string, actorRole MemberRole, invitationID, linkOrigin string) (*Invitation, string, error) {
	if !actorRole.IsAtLeast(RoleAdmin) {
		return nil, "", domain.ErrForbidden
	}
	rawToken, err := security.GenerateOpaqueToken(24)
	if err != nil {
		return nil, "", err
	}
	expiresAt := time.Now().UTC().Add(s.cfg.InvitationTokenTTL)

	var inv *Invitation
	var tenant *Tenant
	var inviter interface{ DisplayName() string }
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		inv, err = s.repo.GetInvitationByID(ctx, tenantID, invitationID)
		if err != nil {
			return err
		}
		// Same guard as sending one in the first place: an Admin may only
		// manage Staff invitations.
		if inv.Role == RoleAdmin && actorRole != RoleOwner {
			return domain.ErrForbidden
		}
		if inv.Status != InvitationStatusPending {
			return domain.ErrInvalidState
		}
		if err := s.repo.RotateInvitationToken(ctx, tenantID, invitationID, security.HashToken(rawToken), expiresAt); err != nil {
			return err
		}
		inv.ExpiresAt = expiresAt
		if tenant, err = s.repo.GetTenantByID(ctx, tenantID); err != nil {
			return err
		}
		u, err := s.users.GetUserByID(ctx, actorUserID)
		if err != nil {
			return err
		}
		inviter = u
		return s.recordAudit(ctx, &tenantID, &actorUserID, nil, audit.ActionInvitationResent,
			map[string]any{"invitation_id": invitationID, "email": inv.Email})
	})
	if err != nil {
		return nil, "", err
	}

	_ = s.mailer.SendInvitationEmail(ctx, inv.Email, tenant.Name, inviter.DisplayName(), invitationLink(linkOrigin, rawToken))
	return inv, rawToken, nil
}

// PreviewInvitation implements invitepreview.Source for workspace
// invitations.
func (s *Service) PreviewInvitation(ctx context.Context, rawToken string) (*invitepreview.Preview, error) {
	inv, err := s.repo.GetInvitationByTokenHash(ctx, security.HashToken(rawToken))
	if err != nil {
		return nil, err
	}
	tenant, err := s.repo.GetTenantByID(ctx, inv.TenantID)
	if err != nil {
		return nil, err
	}
	return &invitepreview.Preview{
		Kind:      invitepreview.KindWorkspace,
		Name:      tenant.Name,
		Role:      string(inv.Role),
		Email:     inv.Email,
		Status:    invitepreview.ResolveStatus(string(inv.Status), inv.ExpiresAt, time.Now().UTC()),
		ExpiresAt: inv.ExpiresAt,
	}, nil
}

// invitationLink is where the invitee lands: the /invite/{token} page of
// the frontend the inviter was using. Workspace and event invitations share
// this shape; the page asks the API which kind the token is.
func invitationLink(origin, token string) string {
	return strings.TrimRight(origin, "/") + "/invite/" + token
}
