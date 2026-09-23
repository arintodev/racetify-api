package tenant

import (
	"net/http"
	"strings"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
)

type Handler struct {
	tenants *Service
}

func NewHandler(tenants *Service) *Handler {
	return &Handler{tenants: tenants}
}

type createTenantRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Create handles POST /api/v1/tenants - Tenant/Organizer registration.
// Requires RequireUserAuth only (no tenant context yet, by definition).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, _ := reqctx.UserID(r.Context())

	var req createTenantRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "name is required.")
		return
	}

	tenant, err := h.tenants.CreateTenant(r.Context(), userID, req.Name, req.Slug)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, tenantResponse(tenant))
}

// ListMine handles GET /api/v1/tenants/me - every tenant the caller
// belongs to, for a tenant-switcher UI. Requires RequireUserAuth only.
func (h *Handler) ListMine(w http.ResponseWriter, r *http.Request) {
	userID, _ := reqctx.UserID(r.Context())
	tenants, err := h.tenants.ListTenantsForUser(r.Context(), userID)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]TenantWithRoleDTO, 0, len(tenants))
	for _, t := range tenants {
		out = append(out, tenantWithRoleResponse(t))
	}
	respond.JSON(w, http.StatusOK, out)
}

// ListMembers handles GET /api/v1/members. Requires
// RequireTenantForUser (any active role). Paginated: ?limit=&cursor=.
func (h *Handler) ListMembers(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	page, err := h.tenants.ListMembers(r.Context(), tenantID, respond.PageParamsFromRequest(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]MemberDTO, 0, len(page.Items))
	for i := range page.Items {
		out = append(out, memberResponse(&page.Items[i]))
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[MemberDTO]{Items: out, NextCursor: page.NextCursor})
}

type inviteStaffRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"` // "admin" | "staff"
}

// InviteStaff handles POST /api/v1/invitations. Requires
// RequireTenantForUser + RequireRole(RoleAdmin).
func (h *Handler) InviteStaff(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())

	var req inviteStaffRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "email is required.")
		return
	}

	inv, rawToken, err := h.tenants.InviteStaff(r.Context(), tenantID, actorUserID, MemberRole(actorRoleStr), MemberRole(req.Role), req.Email)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, InvitationCreatedDTO{
		InvitationDTO: invitationResponse(inv),
		Token:         rawToken,
	})
}

// ListInvitations handles GET /api/v1/invitations.
// Requires RequireTenantForUser + RequireRole(RoleAdmin). Paginated:
// ?limit=&cursor=.
func (h *Handler) ListInvitations(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	page, err := h.tenants.ListInvitations(r.Context(), tenantID, respond.PageParamsFromRequest(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]InvitationDTO, 0, len(page.Items))
	for i := range page.Items {
		out = append(out, invitationResponse(&page.Items[i]))
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[InvitationDTO]{Items: out, NextCursor: page.NextCursor})
}

// RevokeInvitation handles DELETE /api/v1/invitations/{id}.
// Requires RequireTenantForUser + RequireRole(RoleAdmin).
func (h *Handler) RevokeInvitation(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())
	invitationID := r.PathValue("id")

	if err := h.tenants.RevokeInvitation(r.Context(), tenantID, actorUserID, MemberRole(actorRoleStr), invitationID); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

type acceptInvitationRequest struct {
	Token string `json:"token"`
}

// AcceptInvitation handles POST /api/v1/invitations/accept. Requires
// RequireUserAuth only - the invitee redeems the token as themselves, no
// tenant context exists yet (that is what accepting grants).
func (h *Handler) AcceptInvitation(w http.ResponseWriter, r *http.Request) {
	userID, _ := reqctx.UserID(r.Context())

	var req acceptInvitationRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if req.Token == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "token is required.")
		return
	}

	member, err := h.tenants.AcceptInvitation(r.Context(), req.Token, userID)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, memberResponse(member))
}

type setTenantStatusRequest struct {
	Status string  `json:"status"` // "active" | "rejected" | "suspended"
	Reason *string `json:"reason"`
}

// SetStatus handles PATCH /api/v1/admin/tenants/{id}/status - the Platform
// Super Admin "Platform Verification" capability (PRD: "verifikasi
// organizer"): approve, reject, or suspend a tenant. Requires
// RequireSuperAdmin (router-level admission control); actorIsSuperAdmin is
// re-checked inside Service.SetTenantStatus as the real enforcement.
func (h *Handler) SetStatus(w http.ResponseWriter, r *http.Request) {
	actorUserID, _ := reqctx.UserID(r.Context())
	tenantID := r.PathValue("id")

	var req setTenantStatusRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Status) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "status is required.")
		return
	}

	tenant, err := h.tenants.SetTenantStatus(r.Context(), actorUserID, reqctx.IsSuperAdmin(r.Context()), tenantID, TenantStatus(req.Status), req.Reason)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, tenantResponse(tenant))
}

// ---- folded in from the former internal/httpapi/handlers/resource_handler.go
// (see docs/phase0-refactor-plan.md §6): a Phase 0 demo endpoint proving a
// single tenant-scoped resource is reachable, and correctly isolated,
// under both auth models this codebase supports: a user session and an
// M2M (client_credentials) token. Mounted twice in routes.go: once behind
// RequireUserAuth+RequireTenantForUser, once behind
// RequireM2MAuth+RequireTenantForM2M+RequireScope(tenant:read).

// Summary handles GET /api/v1/tenant/summary and GET /api/v1/m2m/tenant/summary.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := reqctx.TenantID(r.Context())
	if !ok {
		respond.Error(w, http.StatusBadRequest, "tenant_required", "No tenant context resolved for this request.")
		return
	}
	summary, err := h.tenants.GetTenantSummary(r.Context(), tenantID)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, tenantSummaryDTO{
		TenantID:         summary.TenantID,
		TenantName:       summary.TenantName,
		OAuthClientCount: summary.OAuthClientCount,
	})
}
