package event

import "time"

// AssignmentLabel is display-only ('crew' | 'volunteer' | 'photographer' |
// 'timing') - EventAssignment.Capabilities is what authorization actually
// checks (docs/event-crew-access-plan.md §2.4). Plain string, no DB-level
// CHECK constraint - see 0001_core.up.sql's doc comment for why the legal
// set is enforced in Go only.
type AssignmentLabel string

const (
	AssignmentLabelCrew         AssignmentLabel = "crew"
	AssignmentLabelVolunteer    AssignmentLabel = "volunteer"
	AssignmentLabelPhotographer AssignmentLabel = "photographer"
	AssignmentLabelTiming       AssignmentLabel = "timing"
)

func (l AssignmentLabel) Valid() bool {
	switch l {
	case AssignmentLabelCrew, AssignmentLabelVolunteer, AssignmentLabelPhotographer, AssignmentLabelTiming:
		return true
	}
	return false
}

type AssignmentStatus string

const (
	AssignmentStatusActive  AssignmentStatus = "active"
	AssignmentStatusRevoked AssignmentStatus = "revoked"
)

// EventAssignment grants user_id narrow, capability-scoped access to
// exactly one Event, independent of whether that user is a tenant_members
// row anywhere - see docs/event-crew-access-plan.md §2.1-§2.2 for why this
// is deliberately keyed off users.id rather than tenant_members.id: a
// purely external crew/volunteer (a freelance RPC operator, a one-day
// volunteer) never needs a tenant_members row to hold one of these.
type EventAssignment struct {
	ID           string
	TenantID     string
	EventID      string
	UserID       string
	Label        AssignmentLabel
	Capabilities []string
	Status       AssignmentStatus
	AssignedBy   string
	ExpiresAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (a *EventAssignment) HasCapability(capability string) bool {
	for _, c := range a.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// IsActive reports whether this grant is currently usable: status must be
// 'active' and, if ExpiresAt is set, not yet in the past.
func (a *EventAssignment) IsActive(now time.Time) bool {
	if a.Status != AssignmentStatusActive {
		return false
	}
	if a.ExpiresAt != nil && now.After(*a.ExpiresAt) {
		return false
	}
	return true
}

// EventAssignmentWithEvent pairs an EventAssignment with the event's own
// name/slug - the shape GET /api/v1/me/event-assignments needs (Repository.
// MyAssignments), since that endpoint has no event_id in its URL to look
// the event back up by.
type EventAssignmentWithEvent struct {
	EventAssignment
	EventName string
	EventSlug string
}

type EventInvitationStatus string

const (
	EventInvitationStatusPending  EventInvitationStatus = "pending"
	EventInvitationStatusAccepted EventInvitationStatus = "accepted"
	EventInvitationStatusRevoked  EventInvitationStatus = "revoked"
)

// EventInvitation is the pending-offer counterpart to EventAssignment, for
// an email with no Racetify account yet (docs/event-crew-access-plan.md
// §3) - event_assignments.user_id is NOT NULL, so there is no row it can
// represent until accepted. Mirrors internal/tenant.Invitation's shape,
// scoped to one event + a capability set instead of a tenant-wide role.
type EventInvitation struct {
	ID       string
	TenantID string
	EventID  string
	Email    string
	Label    AssignmentLabel
	// Capabilities the resulting EventAssignment will carry once accepted.
	Capabilities []string
	TokenHash    string
	InvitedBy    string
	Status       EventInvitationStatus
	// ExpiresAt is this invite TOKEN's own expiry - how long the link is
	// valid before it must be accepted, mirroring invitations.expires_at
	// exactly. AssignmentExpiresAt is unrelated: the "berlaku sampai" the
	// resulting EventAssignment itself should carry, independent of when
	// the invite link expires.
	ExpiresAt           time.Time
	AssignmentExpiresAt *time.Time
	AcceptedAt          *time.Time
	CreatedAt           time.Time
}

func (i *EventInvitation) IsExpired(now time.Time) bool {
	return now.After(i.ExpiresAt)
}
