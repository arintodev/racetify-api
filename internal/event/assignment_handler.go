package event

import (
	"net/http"
	"strings"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

type assignToEventRequest struct {
	Email        string   `json:"email"`
	Label        string   `json:"label"`
	Capabilities []string `json:"capabilities"`
	ExpiresAt    *string  `json:"expires_at"`
}

// CreateAssignment handles POST /api/v1/events/{id}/assignments. Requires
// RequireTenantForUser + RequireRole(RoleStaff) - assigning crew to one's
// own event is "operasional event tertentu", not a tenant-wide
// administrative action (event-crew-access-plan.md §5, mirroring
// phase1-api-plan.md §4's own Staff-gate rationale). Responds 201 with
// either an EventAssignmentDTO (email already had an account) or an
// EventInvitationCreatedDTO (a pending invite was issued instead) -
// exactly one is ever populated, see Service.AssignToEvent's doc comment.
func (h *Handler) CreateAssignment(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())
	eventID := r.PathValue("id")

	var req assignToEventRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "email is required.")
		return
	}

	expiresAt, ok := parseOptionalDate(w, "expires_at", req.ExpiresAt)
	if !ok {
		return
	}

	assignment, inv, rawToken, err := h.events.AssignToEvent(
		r.Context(), tenantID, eventID, actorUserID, rbac.MemberRole(actorRoleStr),
		req.Email, AssignmentLabel(req.Label), req.Capabilities, expiresAt,
	)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	if assignment != nil {
		respond.JSON(w, http.StatusCreated, assignmentResponse(assignment))
		return
	}
	respond.JSON(w, http.StatusCreated, EventInvitationCreatedDTO{
		EventInvitationDTO: eventInvitationResponse(inv),
		Token:              rawToken,
	})
}

// ListAssignments handles GET /api/v1/events/{id}/assignments. Requires
// RequireTenantForUser + RequireRole(RoleStaff). Paginated: ?limit=&cursor=.
func (h *Handler) ListAssignments(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	eventID := r.PathValue("id")

	page, err := h.events.ListAssignments(r.Context(), tenantID, eventID, respond.PageParamsFromRequest(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]EventAssignmentDTO, 0, len(page.Items))
	for i := range page.Items {
		out = append(out, assignmentResponse(&page.Items[i]))
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[EventAssignmentDTO]{Items: out, NextCursor: page.NextCursor})
}

type updateAssignmentRequest struct {
	Label        *string  `json:"label"`
	Capabilities []string `json:"capabilities"`
	ExpiresAt    *string  `json:"expires_at"`
	ClearExpiry  bool     `json:"clear_expiry"`
}

// UpdateAssignment handles PATCH /api/v1/events/{id}/assignments/{assignmentId}.
// Requires RequireTenantForUser + RequireRole(RoleStaff).
func (h *Handler) UpdateAssignment(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())
	eventID := r.PathValue("id")
	assignmentID := r.PathValue("assignmentId")

	var req updateAssignmentRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}

	patch := AssignmentPatch{Capabilities: req.Capabilities, ClearExpiry: req.ClearExpiry}
	if req.Label != nil {
		l := AssignmentLabel(*req.Label)
		patch.Label = &l
	}
	if !req.ClearExpiry {
		expiresAt, ok := parseOptionalDate(w, "expires_at", req.ExpiresAt)
		if !ok {
			return
		}
		patch.ExpiresAt = expiresAt
	}

	assignment, err := h.events.UpdateAssignment(r.Context(), tenantID, eventID, actorUserID, rbac.MemberRole(actorRoleStr), assignmentID, patch)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, assignmentResponse(assignment))
}

// RevokeAssignment handles DELETE /api/v1/events/{id}/assignments/{assignmentId}
// - a soft revoke (status -> 'revoked'), never a hard delete (Repository.
// RevokeAssignment's doc comment). Requires RequireTenantForUser +
// RequireRole(RoleStaff).
func (h *Handler) RevokeAssignment(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())
	eventID := r.PathValue("id")
	assignmentID := r.PathValue("assignmentId")

	if err := h.events.RevokeAssignment(r.Context(), tenantID, eventID, actorUserID, rbac.MemberRole(actorRoleStr), assignmentID); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

// MyAssignmentOnEvent handles GET /api/v1/events/{id}/assignments/me -
// "what am I allowed to do on this event". Requires only
// RequireEventAccess("") (routes.go) - deliberately no specific
// capability, since discovering one's own capabilities is exactly the
// point, and both of RequireEventAccess's paths already stashed
// everything this handler needs into context (MemberRole for an internal
// Staff/Admin/Owner, EventCapabilities for an externally-assigned crew/
// volunteer) - no second database round trip.
func (h *Handler) MyAssignmentOnEvent(w http.ResponseWriter, r *http.Request) {
	eventID := r.PathValue("id")

	if roleStr, ok := reqctx.MemberRole(r.Context()); ok {
		respond.JSON(w, http.StatusOK, MyEventAccessDTO{
			EventID: eventID, Via: "tenant_role", Role: roleStr, Capabilities: AllCapabilities,
		})
		return
	}
	capabilities, _ := reqctx.EventCapabilities(r.Context())
	respond.JSON(w, http.StatusOK, MyEventAccessDTO{
		EventID: eventID, Via: "assignment", Capabilities: capabilities,
	})
}

type acceptEventInvitationRequest struct {
	Token string `json:"token"`
}

// AcceptEventInvitation handles POST /api/v1/events/invitations/accept.
// Requires RequireUserAuth only - the invitee redeems the token as
// themselves, no tenant context exists yet (that is what accepting
// grants), mirroring internal/tenant's POST /api/v1/invitations/accept
// exactly.
func (h *Handler) AcceptEventInvitation(w http.ResponseWriter, r *http.Request) {
	userID, _ := reqctx.UserID(r.Context())

	var req acceptEventInvitationRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if req.Token == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "token is required.")
		return
	}

	assignment, err := h.events.AcceptEventInvitation(r.Context(), req.Token, userID)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, assignmentResponse(assignment))
}

// MyEventAssignments handles GET /api/v1/me/event-assignments - every
// event the caller is assigned to, across every tenant (event-crew-
// access-plan.md §5, event-crew-access-ux.md §3.1's landing picker).
// Requires RequireUserAuth only, same "no tenant context" posture as
// AcceptEventInvitation above: the caller may not be a tenant_members row
// anywhere at all.
func (h *Handler) MyEventAssignments(w http.ResponseWriter, r *http.Request) {
	userID, _ := reqctx.UserID(r.Context())

	assignments, err := h.events.MyAssignments(r.Context(), userID)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]MyEventAssignmentDTO, 0, len(assignments))
	for i := range assignments {
		out = append(out, myAssignmentResponse(&assignments[i]))
	}
	respond.JSON(w, http.StatusOK, out)
}
