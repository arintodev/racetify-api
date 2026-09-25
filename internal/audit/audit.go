// Package audit is an append-only, cross-cutting log of security-relevant
// actions. It is called from every bounded context's service layer but
// owns no invariant any single context enforces, so it lives on its own
// rather than inside any one of them - the same shape Phase 1's planned
// internal/jobqueue takes ("top-level, app-orchestration, not raw infra").
package audit

import "time"

// Log is an append-only record of a security-relevant action, used by
// Platform Super Admin ("audit logs global") and, tenant-scoped, by Tenant
// Owners reviewing their own workspace activity.
type Log struct {
	ID            string
	TenantID      *string // nil = platform-level event
	ActorUserID   *string
	ActorClientID *string
	Action        string
	Metadata      map[string]any
	CreatedAt     time.Time
}

// Common action names, kept centralized so services/tests don't rely on
// string literals scattered across the codebase.
const (
	ActionUserRegistered      = "user.registered"
	ActionUserLoggedIn        = "user.logged_in"
	ActionUserLoggedOut       = "user.logged_out"
	ActionUserEmailVerified   = "user.email_verified"
	ActionUserSwitchedTenant  = "user.switched_tenant"
	ActionTenantCreated       = "tenant.created"
	ActionTenantStatusChanged = "tenant.status_changed"
	ActionInvitationCreated   = "invitation.created"
	ActionInvitationAccepted  = "invitation.accepted"
	ActionInvitationRevoked   = "invitation.revoked"
	ActionInvitationResent    = "invitation.resent"
	ActionMemberRoleChanged   = "member.role_changed"
	ActionMemberRemoved       = "member.removed"
	ActionOAuthClientCreated  = "oauth_client.created"
	ActionOAuthClientRevoked  = "oauth_client.revoked"
	ActionOAuthTokenIssued    = "oauth_token.issued"

	ActionObjectUploadRequested = "object.upload_requested"
	ActionObjectStored          = "object.stored"

	// Phase 1 (docs/phase1-api-plan.md §2) - one constant per action named
	// there, appended rather than reorganized so the Phase 0 block above
	// stays untouched.
	ActionEventCreated       = "event.created"
	ActionEventStatusChanged = "event.status_changed"
	ActionRaceCreated        = "race.created"

	ActionParticipantCreated         = "participant.created"
	ActionParticipantUpdated         = "participant.updated"
	ActionParticipantDeleted         = "participant.deleted"
	ActionParticipantExported        = "participant.exported"
	ActionTeamCreated                = "team.created"
	ActionTeamUpdated                = "team.updated"
	ActionTeamDeleted                = "team.deleted"
	ActionParticipantImportStarted   = "participant.import_started"
	ActionParticipantImportCompleted = "participant.import_completed"

	ActionAlbumCreated = "album.created"
	ActionAlbumUpdated = "album.updated"
	ActionAlbumDeleted = "album.deleted"
	ActionPhotoDeleted = "photo.deleted"

	ActionTemplateUploaded = "template.uploaded"
	ActionTemplateUpdated  = "template.updated"
	ActionTemplateDeleted  = "template.deleted"

	ActionBibGenerationRequested = "bib_generation.requested"
	ActionCertificateGenerated   = "certificate.generated"

	ActionPhotoUploaded     = "photo.uploaded"
	ActionPhotoTagCorrected = "photo_tag.corrected"

	// Event-scoped crew/volunteer access (docs/event-crew-access-plan.md).
	ActionEventAssignmentCreated  = "event_assignment.created"
	ActionEventAssignmentUpdated  = "event_assignment.updated"
	ActionEventAssignmentRevoked  = "event_assignment.revoked"
	ActionEventAssignmentInvited  = "event_assignment.invited"
	ActionEventAssignmentAccepted = "event_assignment.accepted"
)
