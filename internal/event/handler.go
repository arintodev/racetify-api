package event

import (
	"net/http"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

type Handler struct {
	events *Service
}

func NewHandler(events *Service) *Handler {
	return &Handler{events: events}
}

// ==================== events ====================

type createEventRequest struct {
	Name      string  `json:"name"`
	Slug      string  `json:"slug"`
	Venue     *string `json:"venue"`
	StartDate *string `json:"start_date"`
	EndDate   *string `json:"end_date"`
}

// Create handles POST /api/v1/events. Requires RequireTenantForUser +
// RequireRole(RoleAdmin) - see routes.go.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())

	var req createEventRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "name is required.")
		return
	}

	startDate, ok := parseOptionalDate(w, "start_date", req.StartDate)
	if !ok {
		return
	}
	endDate, ok := parseOptionalDate(w, "end_date", req.EndDate)
	if !ok {
		return
	}

	ev, err := h.events.CreateEvent(r.Context(), tenantID, actorUserID, rbac.MemberRole(actorRoleStr), req.Name, req.Slug, req.Venue, startDate, endDate)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, eventResponse(ev))
}

// List handles GET /api/v1/events and GET /api/v1/m2m/events. Requires
// RequireTenantForUser + RequireRole(RoleStaff) for the former,
// RequireTenantForM2M + RequireScope(events:read) for the latter - see
// routes.go. Paginated: ?limit=&cursor=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	page, err := h.events.ListEvents(r.Context(), tenantID, respond.PageParamsFromRequest(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]EventDTO, 0, len(page.Items))
	for i := range page.Items {
		out = append(out, eventResponse(&page.Items[i]))
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[EventDTO]{Items: out, NextCursor: page.NextCursor})
}

// Get handles GET /api/v1/events/{id} and GET /api/v1/m2m/events/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	ev, err := h.events.GetEvent(r.Context(), tenantID, r.PathValue("id"))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, eventResponse(ev))
}

type updateEventRequest struct {
	Name               *string `json:"name"`
	Slug               *string `json:"slug"`
	Venue              *string `json:"venue"`
	StartDate          *string `json:"start_date"`
	EndDate            *string `json:"end_date"`
	LogoStorageID      *string `json:"logo_storage_id"`
	ThumbnailStorageID *string `json:"thumbnail_storage_id"`
}

// Update handles PATCH /api/v1/events/{id}. Requires RequireTenantForUser
// + RequireRole(RoleAdmin).
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())

	var req updateEventRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}

	patch := EventPatch{
		Name: req.Name, Slug: req.Slug, Venue: req.Venue,
		LogoStorageID: req.LogoStorageID, ThumbnailStorageID: req.ThumbnailStorageID,
	}
	if req.StartDate != nil {
		d, ok := parseOptionalDate(w, "start_date", req.StartDate)
		if !ok {
			return
		}
		patch.StartDate = d
	}
	if req.EndDate != nil {
		d, ok := parseOptionalDate(w, "end_date", req.EndDate)
		if !ok {
			return
		}
		patch.EndDate = d
	}

	ev, err := h.events.UpdateEvent(r.Context(), tenantID, actorUserID, rbac.MemberRole(actorRoleStr), r.PathValue("id"), patch)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, eventResponse(ev))
}

type setEventStatusRequest struct {
	Status string `json:"status"` // "draft" | "published" | "archived"
}

// SetStatus handles PATCH /api/v1/events/{id}/status - the transition that
// makes the Runner Portal (once built) resolve this event. Requires
// RequireTenantForUser + RequireRole(RoleAdmin).
func (h *Handler) SetStatus(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())

	var req setEventStatusRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Status) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "status is required.")
		return
	}

	ev, err := h.events.SetEventStatus(r.Context(), tenantID, actorUserID, rbac.MemberRole(actorRoleStr), r.PathValue("id"), EventStatus(req.Status))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, eventResponse(ev))
}

// ==================== races ====================

type createRaceRequest struct {
	Name       string   `json:"name"`
	Slug       string   `json:"slug"`
	DistanceKM *float64 `json:"distance_km"`
}

// CreateRace handles POST /api/v1/events/{id}/races. Requires
// RequireTenantForUser + RequireRole(RoleAdmin).
func (h *Handler) CreateRace(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())
	eventID := r.PathValue("id")

	var req createRaceRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "name is required.")
		return
	}

	race, err := h.events.CreateRace(r.Context(), tenantID, eventID, actorUserID, rbac.MemberRole(actorRoleStr), req.Name, req.Slug, req.DistanceKM)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, raceResponse(race))
}

// ListRaces handles GET /api/v1/events/{id}/races. Requires
// RequireTenantForUser + RequireRole(RoleStaff).
func (h *Handler) ListRaces(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	eventID := r.PathValue("id")

	races, err := h.events.ListRaces(r.Context(), tenantID, eventID)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]RaceDTO, 0, len(races))
	for i := range races {
		out = append(out, raceResponse(&races[i]))
	}
	respond.JSON(w, http.StatusOK, out)
}

type updateRaceRequest struct {
	Name       *string  `json:"name"`
	Slug       *string  `json:"slug"`
	DistanceKM *float64 `json:"distance_km"`
}

// UpdateRace handles PATCH /api/v1/events/{id}/races/{raceId}. Requires
// RequireTenantForUser + RequireRole(RoleAdmin).
func (h *Handler) UpdateRace(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())
	eventID := r.PathValue("id")
	raceID := r.PathValue("raceId")

	var req updateRaceRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}

	race, err := h.events.UpdateRace(r.Context(), tenantID, eventID, actorUserID, rbac.MemberRole(actorRoleStr), raceID, RacePatch{
		Name: req.Name, Slug: req.Slug, DistanceKM: req.DistanceKM,
	})
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, raceResponse(race))
}

// DeleteRace handles DELETE /api/v1/events/{id}/races/{raceId}. Requires
// RequireTenantForUser + RequireRole(RoleAdmin). Fails with
// invalid_request (400) if participants still reference the race - see
// Repository.DeleteRace's doc comment.
func (h *Handler) DeleteRace(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())
	eventID := r.PathValue("id")
	raceID := r.PathValue("raceId")

	if err := h.events.DeleteRace(r.Context(), tenantID, eventID, actorUserID, rbac.MemberRole(actorRoleStr), raceID); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// parseOptionalDate decodes a "2006-01-02" date-only string, writing a 400
// invalid_request and returning ok=false on a malformed value. A nil input
// (field not supplied) is not an error and returns (nil, true).
func parseOptionalDate(w http.ResponseWriter, field string, raw *string) (result *time.Time, ok bool) {
	if raw == nil {
		return nil, true
	}
	t, err := parseDateOnly(*raw)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", field+" must be a date in YYYY-MM-DD format.")
		return nil, false
	}
	return t, true
}
