package participant

import (
	"encoding/csv"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// maxImportBodyBytes caps an import request body (about 5,000 rows with
// generous headroom).
const maxImportBodyBytes = 8 << 20

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// fail writes err: a participant.Error carries its own status, code and the
// field it concerns; anything else goes through the shared mapping.
func fail(w http.ResponseWriter, err error) {
	var perr *Error
	if errors.As(err, &perr) {
		respond.ErrorWithField(w, perr.Status, perr.Code, perr.Message, perr.Field)
		return
	}
	respond.FromServiceError(w, err)
}

// actor extracts who is calling. tenant is always present once the route's
// gate has run; role is empty for an external crew member (read routes).
func actor(r *http.Request) (tenantID, userID string, role rbac.MemberRole) {
	tenantID, _ = reqctx.TenantID(r.Context())
	userID, _ = reqctx.UserID(r.Context())
	roleStr, _ := reqctx.MemberRole(r.Context())
	return tenantID, userID, rbac.MemberRole(roleStr)
}

func parseFilter(w http.ResponseWriter, r *http.Request) (ListFilter, bool) {
	q := r.URL.Query()
	f := ListFilter{
		RaceID: q.Get("race_id"), TeamID: q.Get("team_id"), Club: q.Get("club"), Query: q.Get("q"),
		NoClub: q.Get("no_club") == "true", Unassigned: q.Get("unassigned") == "true",
	}
	if v := q.Get("status"); v != "" {
		if !Status(v).Valid() {
			respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "status is not valid.", "status")
			return f, false
		}
		f.Status = Status(v)
	}
	if v := q.Get("gender"); v != "" {
		if !Gender(v).Valid() {
			respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "gender must be male or female.", "gender")
			return f, false
		}
		f.Gender = Gender(v)
	}
	if v := q.Get("bib_number"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "bib_number must be a positive number.", "bib_number")
			return f, false
		}
		f.BibNumber = &n
	}
	return f, true
}

// ==================== participants ====================

// List handles GET /events/{id}/participants.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	f, ok := parseFilter(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListParticipants(r.Context(), tenantID, r.PathValue("id"), f, respond.PageParamsFromRequest(r))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]ParticipantDTO, len(page.Items))
	for i := range page.Items {
		out[i] = participantResponse(&page.Items[i])
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[ParticipantDTO]{Items: out, NextCursor: page.NextCursor})
}

// Summary handles GET /events/{id}/participants/summary.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	sum, err := h.svc.Summary(r.Context(), tenantID, r.PathValue("id"), r.URL.Query().Get("race_id"))
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, summaryResponse(sum))
}

// Get handles GET /events/{id}/participants/{pid}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	p, err := h.svc.GetParticipant(r.Context(), tenantID, r.PathValue("id"), r.PathValue("pid"))
	if err != nil {
		fail(w, err)
		return
	}
	dto := participantResponse(p)
	respond.JSON(w, http.StatusOK, dto)
}

type createParticipantRequest struct {
	RaceID string `json:"race_id"`
	participantFields
}

// Create handles POST /events/{id}/participants.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req createParticipantRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	p, err := h.svc.CreateParticipant(r.Context(), tenantID, r.PathValue("id"), userID, role, req.RaceID, req.toInput())
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, participantResponse(p))
}

// Update handles PATCH /events/{id}/participants/{pid}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req updateParticipantRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	p, err := h.svc.UpdateParticipant(r.Context(), tenantID, r.PathValue("id"), userID, role, r.PathValue("pid"), req.toPatch())
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, participantResponse(p))
}

type selectionRequest struct {
	IDs    []string       `json:"ids"`
	Filter *filterRequest `json:"filter"`
	Status *string        `json:"status"`
}

type filterRequest struct {
	RaceID     string `json:"race_id"`
	TeamID     string `json:"team_id"`
	Status     string `json:"status"`
	Gender     string `json:"gender"`
	Club       string `json:"club"`
	NoClub     bool   `json:"no_club"`
	Unassigned bool   `json:"unassigned"`
	Q          string `json:"q"`
}

func (s selectionRequest) toSelection() Selection {
	sel := Selection{IDs: s.IDs}
	if s.Filter != nil {
		f := ListFilter{
			RaceID: s.Filter.RaceID, TeamID: s.Filter.TeamID, Status: Status(s.Filter.Status), Gender: Gender(s.Filter.Gender),
			Club: s.Filter.Club, NoClub: s.Filter.NoClub, Unassigned: s.Filter.Unassigned, Query: s.Filter.Q,
		}
		sel.Filter = &f
	}
	return sel
}

// BulkDelete handles POST /events/{id}/participants/bulk-delete.
func (h *Handler) BulkDelete(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req selectionRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	n, err := h.svc.DeleteParticipants(r.Context(), tenantID, r.PathValue("id"), userID, role, req.toSelection())
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

// Delete handles DELETE /events/{id}/participants/{pid}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	n, err := h.svc.DeleteParticipants(r.Context(), tenantID, r.PathValue("id"), userID, role, Selection{IDs: []string{r.PathValue("pid")}})
	if err != nil {
		fail(w, err)
		return
	}
	if n == 0 {
		respond.Error(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// BulkStatus handles POST /events/{id}/participants/bulk-status.
func (h *Handler) BulkStatus(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req selectionRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	status := ""
	if req.Status != nil {
		status = *req.Status
	}
	n, err := h.svc.SetStatus(r.Context(), tenantID, r.PathValue("id"), userID, role, req.toSelection(), Status(status))
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]int64{"updated": n})
}

// Export handles GET /events/{id}/participants/export: a CSV of everyone
// matching the filter, streamed. Cells that a spreadsheet would read as a
// formula are neutralised.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	f, ok := parseFilter(w, r)
	if !ok {
		return
	}
	var cw *csv.Writer
	err := h.svc.Export(r.Context(), tenantID, r.PathValue("id"), userID, role, f, func(p *Participant, raceName, teamName string) error {
		if cw == nil {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="participants.csv"`)
			cw = csv.NewWriter(w)
			if err := cw.Write(exportHeader); err != nil {
				return err
			}
		}
		return cw.Write(exportRow(p, raceName, teamName))
	})
	if err != nil {
		if cw == nil {
			fail(w, err)
		}
		return
	}
	if cw == nil { // no rows: still a valid CSV
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="participants.csv"`)
		cw = csv.NewWriter(w)
		_ = cw.Write(exportHeader)
	}
	cw.Flush()
}

var exportHeader = []string{
	"bib_number", "first_name", "last_name", "bib_name", "gender", "email", "club",
	"emergency_contact_name", "emergency_contact_phone", "blood_type", "race", "team", "leg_order", "status", "ref_id",
}

func exportRow(p *Participant, raceName, teamName string) []string {
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	leg := ""
	if p.LegOrder != nil {
		leg = strconv.Itoa(*p.LegOrder)
	}
	cells := []string{
		strconv.Itoa(p.BibNumber), p.FirstName, str(p.LastName), str(p.BibName), string(p.Gender), str(p.Email), str(p.Club),
		str(p.EmergencyContactName), str(p.EmergencyContactPhone), str(p.BloodType), raceName, teamName, leg, string(p.Status), str(p.RefID),
	}
	for i, c := range cells {
		cells[i] = neutraliseFormula(c)
	}
	return cells
}

// neutraliseFormula stops a cell from being evaluated as a formula when the
// CSV is opened in a spreadsheet (CSV injection): a leading = + - @ or
// control character is prefixed with an apostrophe.
func neutraliseFormula(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ==================== import ====================

// Import handles POST /events/{id}/participants/import.
func (h *Handler) Import(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBodyBytes)
	var req importRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	res, err := h.svc.Import(r.Context(), tenantID, r.PathValue("id"), userID, role, req.toImportRequest())
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, importResponse(res))
}

// ==================== teams ====================

// ListTeams handles GET /events/{id}/teams.
func (h *Handler) ListTeams(w http.ResponseWriter, r *http.Request) {
	tenantID, _, _ := actor(r)
	q := r.URL.Query()
	f := TeamFilter{RaceID: q.Get("race_id"), Query: q.Get("q")}
	if v := q.Get("gender_category"); v != "" {
		if !GenderCategory(v).Valid() {
			respond.ErrorWithField(w, http.StatusBadRequest, "invalid_request", "gender_category must be male, female or mixed.", "gender_category")
			return
		}
		f.GenderCategory = GenderCategory(v)
	}
	page, err := h.svc.ListTeams(r.Context(), tenantID, r.PathValue("id"), f, respond.PageParamsFromRequest(r))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]TeamDTO, len(page.Items))
	for i := range page.Items {
		out[i] = teamResponse(&page.Items[i])
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[TeamDTO]{Items: out, NextCursor: page.NextCursor})
}

type createTeamRequest struct {
	RaceID         string `json:"race_id"`
	Name           string `json:"name"`
	GenderCategory string `json:"gender_category"`
}

// CreateTeam handles POST /events/{id}/teams.
func (h *Handler) CreateTeam(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req createTeamRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	t, err := h.svc.CreateTeam(r.Context(), tenantID, r.PathValue("id"), userID, role, req.RaceID, req.Name, GenderCategory(req.GenderCategory))
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, teamResponse(&TeamWithMembers{Team: *t}))
}

type updateTeamRequest struct {
	Name           *string `json:"name"`
	GenderCategory *string `json:"gender_category"`
}

// UpdateTeam handles PATCH /events/{id}/teams/{tid}.
func (h *Handler) UpdateTeam(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	var req updateTeamRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}
	var cat *GenderCategory
	if req.GenderCategory != nil {
		c := GenderCategory(*req.GenderCategory)
		cat = &c
	}
	t, err := h.svc.UpdateTeam(r.Context(), tenantID, r.PathValue("id"), userID, role, r.PathValue("tid"), req.Name, cat)
	if err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, teamResponse(&TeamWithMembers{Team: *t}))
}

// DeleteTeam handles DELETE /events/{id}/teams/{tid}?with_members=true.
func (h *Handler) DeleteTeam(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, role := actor(r)
	withMembers := r.URL.Query().Get("with_members") == "true"
	if err := h.svc.DeleteTeam(r.Context(), tenantID, r.PathValue("id"), userID, role, r.PathValue("tid"), withMembers); err != nil {
		fail(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
