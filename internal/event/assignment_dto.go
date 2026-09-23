package event

import "time"

type EventAssignmentDTO struct {
	ID           string     `json:"id"`
	EventID      string     `json:"event_id"`
	UserID       string     `json:"user_id"`
	Label        string     `json:"label"`
	Capabilities []string   `json:"capabilities"`
	Status       string     `json:"status"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func assignmentResponse(a *EventAssignment) EventAssignmentDTO {
	return EventAssignmentDTO{
		ID: a.ID, EventID: a.EventID, UserID: a.UserID, Label: string(a.Label),
		Capabilities: a.Capabilities, Status: string(a.Status), ExpiresAt: a.ExpiresAt, CreatedAt: a.CreatedAt,
	}
}

// MyEventAssignmentDTO is what GET /api/v1/me/event-assignments returns -
// the shape docs/event-crew-access-ux.md §3.1's landing picker needs,
// which is why it carries the event's own name/slug rather than just an
// id (the caller has no tenant context to look that up separately).
type MyEventAssignmentDTO struct {
	EventAssignmentDTO
	EventName string `json:"event_name"`
	EventSlug string `json:"event_slug"`
}

func myAssignmentResponse(a *EventAssignmentWithEvent) MyEventAssignmentDTO {
	return MyEventAssignmentDTO{
		EventAssignmentDTO: assignmentResponse(&a.EventAssignment),
		EventName:          a.EventName,
		EventSlug:          a.EventSlug,
	}
}

type EventInvitationDTO struct {
	ID           string     `json:"id"`
	EventID      string     `json:"event_id"`
	Email        string     `json:"email"`
	Label        string     `json:"label"`
	Capabilities []string   `json:"capabilities"`
	Status       string     `json:"status"`
	ExpiresAt    time.Time  `json:"expires_at"`
	AcceptedAt   *time.Time `json:"accepted_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func eventInvitationResponse(i *EventInvitation) EventInvitationDTO {
	return EventInvitationDTO{
		ID: i.ID, EventID: i.EventID, Email: i.Email, Label: string(i.Label), Capabilities: i.Capabilities,
		Status: string(i.Status), ExpiresAt: i.ExpiresAt, AcceptedAt: i.AcceptedAt, CreatedAt: i.CreatedAt,
	}
}

// EventInvitationCreatedDTO is only ever returned once, from the
// assignment endpoint's new-account branch - same "carries the one-use
// secret token" shape as internal/tenant's InvitationCreatedDTO.
type EventInvitationCreatedDTO struct {
	EventInvitationDTO
	Token string `json:"token"`
}

// MyEventAccessDTO is GET /api/v1/events/{id}/assignments/me's response -
// "what can I, the caller, do on this event" for whichever of
// RequireEventAccess's two paths let them through (access.go). Via is
// "tenant_role" for an internal Staff/Admin/Owner (Role set, Capabilities
// is AllCapabilities) or "assignment" for an externally-assigned crew/
// volunteer (Role empty, Capabilities is their actual grant).
type MyEventAccessDTO struct {
	EventID      string   `json:"event_id"`
	Via          string   `json:"via"`
	Role         string   `json:"role,omitempty"`
	Capabilities []string `json:"capabilities"`
}
