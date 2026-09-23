package tenant

import "time"

type InvitationStatus string

const (
	InvitationStatusPending  InvitationStatus = "pending"
	InvitationStatusAccepted InvitationStatus = "accepted"
	InvitationStatusRevoked  InvitationStatus = "revoked"
	InvitationStatusExpired  InvitationStatus = "expired"
)

// Invitation is the "Tenant Staff Onboarding (Invitation Flow)" token: a
// Tenant Owner/Admin invites a staff/crew member by email; the invitee
// accepts using a unique, single-use, expiring token.
type Invitation struct {
	ID         string
	TenantID   string
	Email      string
	Role       MemberRole // 'admin' or 'staff' - never 'owner'
	TokenHash  string
	InvitedBy  string
	Status     InvitationStatus
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	CreatedAt  time.Time
}

func (i *Invitation) IsExpired(now time.Time) bool {
	return now.After(i.ExpiresAt)
}
