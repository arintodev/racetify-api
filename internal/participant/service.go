package participant

import (
	"context"
	"fmt"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
)

// MaxImportRows bounds one import request. It keeps the request body and the
// single transaction it runs in to a sane size; a larger file is imported in
// several requests.
const MaxImportRows = 5000

// Service implements the participant and team use cases. Every mutating
// method takes the actor's role and re-checks it (defence in depth on top of
// the router-level gate), like the other bounded contexts.
type Service struct {
	db    *database.DB
	repo  *Repository
	audit *audit.Repository
}

func NewService(db *database.DB, repo *Repository, audit *audit.Repository) *Service {
	return &Service{db: db, repo: repo, audit: audit}
}

func (s *Service) recordAudit(ctx context.Context, tenantID, actorUserID, action string, metadata map[string]any) error {
	return s.audit.Record(ctx, &audit.Log{
		ID:          security.MustNewUUIDv4(),
		TenantID:    &tenantID,
		ActorUserID: &actorUserID,
		Action:      action,
		Metadata:    metadata,
		CreatedAt:   time.Now().UTC(),
	})
}

func requireRole(actor, min rbac.MemberRole) error {
	if !actor.IsAtLeast(min) {
		return domain.ErrForbidden
	}
	return nil
}

// raceOf loads the race a participant or team belongs to, proving it is in
// this event.
func (s *Service) raceOf(ctx context.Context, tenantID, eventID, raceID string) (RaceFormat, error) {
	races, err := s.repo.ListRaceFormats(ctx, tenantID, eventID)
	if err != nil {
		return RaceFormat{}, err
	}
	race, ok := races[raceID]
	if !ok {
		return RaceFormat{}, invalid("race_id", "race_id is not a race of this event.")
	}
	return race, nil
}

// ==================== participants ====================

// CreateInput is a create request.
type CreateInput struct {
	RaceID string
	Input  Input
}

// checkTeamJoin enforces the team rules for putting a participant in a
// team: it exists in the same race, has room, keeps the gender category
// valid, and (relay) takes a free leg. excludeID is the participant being
// edited, so they do not count against themselves.
func (s *Service) checkTeamJoin(ctx context.Context, tenantID, eventID string, race RaceFormat, in Input, excludeID string) error {
	if in.TeamID == nil {
		return nil
	}
	team, err := s.repo.GetTeam(ctx, tenantID, eventID, *in.TeamID)
	if err != nil {
		if err == domain.ErrNotFound {
			return invalid("team_id", "team_id is not a team of this event.")
		}
		return err
	}
	if team.RaceID != race.ID {
		return invalid("team_id", "the team belongs to a different race.")
	}
	members, err := s.repo.ListTeamMembers(ctx, tenantID, []string{team.ID})
	if err != nil {
		return err
	}
	size := 0
	if race.TeamSize != nil {
		size = *race.TeamSize
	}
	genders := []Gender{in.Gender}
	count := 1
	for _, m := range members {
		if m.ID == excludeID {
			continue
		}
		count++
		genders = append(genders, m.Gender)
		if in.LegOrder != nil && m.LegOrder != nil && *m.LegOrder == *in.LegOrder {
			return conflict("leg_taken", "leg_order", fmt.Sprintf("leg %d is already taken in this team.", *in.LegOrder))
		}
	}
	if size > 0 && count > size {
		return conflict("team_full", "team_id", fmt.Sprintf("this team already has %d members.", size))
	}
	if cerr, _ := CheckTeamComposition(team.GenderCategory, size, genders); cerr != nil {
		return cerr
	}
	return nil
}

// CreateParticipant registers one person.
func (s *Service) CreateParticipant(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, raceID string, in Input) (*Participant, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return nil, err
	}
	if bad := in.Normalize(); bad != "" {
		return nil, invalid("blood_type", fmt.Sprintf("blood_type %q is not recognised.", bad))
	}

	var created *Participant
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		race, err := s.raceOf(ctx, tenantID, eventID, raceID)
		if err != nil {
			return err
		}
		if verr := in.Validate(race); verr != nil {
			return verr
		}
		if err := s.checkTeamJoin(ctx, tenantID, eventID, race, in, ""); err != nil {
			return err
		}
		now := time.Now().UTC()
		p := &Participant{
			ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID, RaceID: raceID,
			TeamID: in.TeamID, LegOrder: in.LegOrder,
			FirstName: in.FirstName, LastName: in.LastName, BibName: in.BibName, BibNumber: in.BibNumber,
			Gender: in.Gender, RefID: in.RefID, Email: in.Email, Club: in.Club,
			EmergencyContactName: in.EmergencyContactName, EmergencyContactPhone: in.EmergencyContactPhone,
			BloodType: in.BloodType, Status: in.Status, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.CreateParticipant(ctx, p); err != nil {
			return err
		}
		created = p
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionParticipantCreated,
			map[string]any{"participant_id": p.ID, "event_id": eventID, "bib_number": p.BibNumber})
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Service) GetParticipant(ctx context.Context, tenantID, eventID, id string) (*Participant, error) {
	var p *Participant
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		p, err = s.repo.GetParticipant(ctx, tenantID, eventID, id)
		return err
	})
	return p, err
}

func (s *Service) ListParticipants(ctx context.Context, tenantID, eventID string, f ListFilter, page pagination.PageParams) (pagination.Page[Participant], error) {
	var out pagination.Page[Participant]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		out, err = s.repo.ListParticipants(ctx, tenantID, eventID, f, page)
		return err
	})
	return out, err
}

// Patch carries only the fields an update supplied. A nil pointer leaves the
// field unchanged; an empty string clears an optional text field, and for
// TeamID / LegOrder an empty string / zero removes them.
type Patch struct {
	RaceID                *string
	FirstName             *string
	LastName              *string
	BibName               *string
	BibNumber             *int
	Gender                *Gender
	RefID                 *string
	Email                 *string
	Club                  *string
	EmergencyContactName  *string
	EmergencyContactPhone *string
	BloodType             *string
	Status                *Status
	TeamID                *string
	LegOrder              *int
}

func (s *Service) UpdateParticipant(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, id string, patch Patch) (*Participant, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return nil, err
	}
	var updated *Participant
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		p, err := s.repo.GetParticipant(ctx, tenantID, eventID, id)
		if err != nil {
			return err
		}
		in := Input{
			FirstName: p.FirstName, LastName: p.LastName, BibName: p.BibName, BibNumber: p.BibNumber,
			Gender: p.Gender, RefID: p.RefID, Email: p.Email, Club: p.Club,
			EmergencyContactName: p.EmergencyContactName, EmergencyContactPhone: p.EmergencyContactPhone,
			BloodType: p.BloodType, Status: p.Status, TeamID: p.TeamID, LegOrder: p.LegOrder,
		}
		raceID := p.RaceID
		if patch.RaceID != nil && *patch.RaceID != p.RaceID {
			// Moving between races changes which format rules apply, so
			// the team assignment does not carry over.
			raceID = *patch.RaceID
			in.TeamID, in.LegOrder = nil, nil
		}
		if patch.FirstName != nil {
			in.FirstName = *patch.FirstName
		}
		if patch.LastName != nil {
			in.LastName = patch.LastName
		}
		if patch.BibName != nil {
			in.BibName = patch.BibName
		}
		if patch.BibNumber != nil {
			in.BibNumber = *patch.BibNumber
		}
		if patch.Gender != nil {
			in.Gender = *patch.Gender
		}
		if patch.RefID != nil {
			in.RefID = patch.RefID
		}
		if patch.Email != nil {
			in.Email = patch.Email
		}
		if patch.Club != nil {
			in.Club = patch.Club
		}
		if patch.EmergencyContactName != nil {
			in.EmergencyContactName = patch.EmergencyContactName
		}
		if patch.EmergencyContactPhone != nil {
			in.EmergencyContactPhone = patch.EmergencyContactPhone
		}
		if patch.BloodType != nil {
			in.BloodType = patch.BloodType
		}
		if patch.Status != nil {
			in.Status = *patch.Status
		}
		if patch.TeamID != nil {
			in.TeamID = patch.TeamID
		}
		if patch.LegOrder != nil {
			if *patch.LegOrder == 0 {
				in.LegOrder = nil
			} else {
				in.LegOrder = patch.LegOrder
			}
		}
		if bad := in.Normalize(); bad != "" {
			return invalid("blood_type", fmt.Sprintf("blood_type %q is not recognised.", bad))
		}

		race, err := s.raceOf(ctx, tenantID, eventID, raceID)
		if err != nil {
			return err
		}
		// A participant already left without a team (its team was deleted
		// with "keep members") may be edited without being forced into one.
		lenient := race.IsTeam() && in.TeamID == nil && p.TeamID == nil
		if verr := in.Validate(race); verr != nil && !(lenient && verr.Field == "team_id") {
			return verr
		}
		if err := s.checkTeamJoin(ctx, tenantID, eventID, race, in, p.ID); err != nil {
			return err
		}

		p.RaceID, p.TeamID, p.LegOrder = raceID, in.TeamID, in.LegOrder
		p.FirstName, p.LastName, p.BibName, p.BibNumber = in.FirstName, in.LastName, in.BibName, in.BibNumber
		p.Gender, p.RefID, p.Email, p.Club = in.Gender, in.RefID, in.Email, in.Club
		p.EmergencyContactName, p.EmergencyContactPhone = in.EmergencyContactName, in.EmergencyContactPhone
		p.BloodType, p.Status = in.BloodType, in.Status
		if err := s.repo.UpdateParticipant(ctx, p); err != nil {
			return err
		}
		updated = p
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionParticipantUpdated,
			map[string]any{"participant_id": p.ID, "event_id": eventID})
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Selection picks participants for a bulk action: either explicit ids, or
// everything matching a filter (so "select all" over a filtered list of
// thousands does not have to send thousands of ids).
type Selection struct {
	IDs    []string
	Filter *ListFilter
}

func (sel Selection) clause(tenantID, eventID string) (string, []any, error) {
	switch {
	case len(sel.IDs) > 0 && sel.Filter == nil:
		clause, args := ListFilter{}.where(tenantID, eventID)
		clause, args = idsClause(clause, args, sel.IDs)
		return clause, args, nil
	case sel.Filter != nil && len(sel.IDs) == 0:
		clause, args := sel.Filter.where(tenantID, eventID)
		return clause, args, nil
	}
	return "", nil, invalid("selection", "give either ids or filter.")
}

// DeleteParticipants removes the selected participants (Admin+).
func (s *Service) DeleteParticipants(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, sel Selection) (int64, error) {
	if err := requireRole(actorRole, rbac.RoleAdmin); err != nil {
		return 0, err
	}
	clause, args, err := sel.clause(tenantID, eventID)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		if n, err = s.repo.DeleteParticipants(ctx, clause, args); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionParticipantDeleted,
			map[string]any{"event_id": eventID, "count": n})
	})
	return n, err
}

// SetStatus sets registered / dns on the selected participants. Finisher and
// DNF come from results, not from here.
func (s *Service) SetStatus(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, sel Selection, status Status) (int64, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return 0, err
	}
	if status != StatusRegistered && status != StatusDNS {
		return 0, invalid("status", "status must be registered or dns.")
	}
	clause, args, err := sel.clause(tenantID, eventID)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		if n, err = s.repo.SetParticipantsStatus(ctx, clause, args, status); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionParticipantUpdated,
			map[string]any{"event_id": eventID, "count": n, "status": string(status)})
	})
	return n, err
}

// Summary is what the statistics cards and the club dropdown need.
type Summary struct {
	Total    int
	ByStatus map[Status]int
	ByGender map[string]int
	ByRace   map[string]int
	// TeamsByRace is how many teams each team race has.
	TeamsByRace map[string]int
	Clubs       []string
}

func (s *Service) Summary(ctx context.Context, tenantID, eventID, raceID string) (*Summary, error) {
	out := &Summary{ByStatus: map[Status]int{}, ByGender: map[string]int{}, ByRace: map[string]int{}}
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		counts, err := s.repo.SummaryCounts(ctx, tenantID, eventID, raceID)
		if err != nil {
			return err
		}
		for _, c := range counts {
			out.Total += c.Count
			out.ByStatus[c.Status] += c.Count
			out.ByRace[c.RaceID] += c.Count
			if c.Gender != "" {
				out.ByGender[c.Gender] += c.Count
			}
		}
		if out.TeamsByRace, err = s.repo.TeamCounts(ctx, tenantID, eventID); err != nil {
			return err
		}
		out.Clubs, err = s.repo.ListClubs(ctx, tenantID, eventID, raceID)
		return err
	})
	return out, err
}

// Export streams every participant matching f to fn, along with the names
// of the race and team they are in, and records the export (personal data
// leaves the system here).
func (s *Service) Export(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, f ListFilter, fn func(p *Participant, raceName, teamName string) error) error {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return err
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionParticipantExported,
			map[string]any{"event_id": eventID}); err != nil {
			return err
		}
		races, err := s.repo.ListRaceFormats(ctx, tenantID, eventID)
		if err != nil {
			return err
		}
		teams, err := s.repo.ListAllTeams(ctx, tenantID, eventID)
		if err != nil {
			return err
		}
		teamNames := make(map[string]string, len(teams))
		for _, t := range teams {
			teamNames[t.ID] = t.Name
		}
		return s.repo.ForEachParticipant(ctx, tenantID, eventID, f, func(p *Participant) error {
			teamName := ""
			if p.TeamID != nil {
				teamName = teamNames[*p.TeamID]
			}
			return fn(p, races[p.RaceID].Name, teamName)
		})
	})
}

// ==================== teams ====================

// TeamWithMembers is a team and who is in it.
type TeamWithMembers struct {
	Team
	Members []TeamMember
}

func (s *Service) ListTeams(ctx context.Context, tenantID, eventID string, f TeamFilter, page pagination.PageParams) (pagination.Page[TeamWithMembers], error) {
	var out pagination.Page[TeamWithMembers]
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		teams, err := s.repo.ListTeams(ctx, tenantID, eventID, f, page)
		if err != nil {
			return err
		}
		ids := make([]string, len(teams.Items))
		for i, t := range teams.Items {
			ids[i] = t.ID
		}
		members, err := s.repo.ListTeamMembers(ctx, tenantID, ids)
		if err != nil {
			return err
		}
		byTeam := map[string][]TeamMember{}
		for _, m := range members {
			byTeam[m.TeamID] = append(byTeam[m.TeamID], m)
		}
		out.NextCursor = teams.NextCursor
		for _, t := range teams.Items {
			out.Items = append(out.Items, TeamWithMembers{Team: t, Members: byTeam[t.ID]})
		}
		return nil
	})
	return out, err
}

func (s *Service) CreateTeam(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, raceID, name string, category GenderCategory) (*Team, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return nil, err
	}
	var team *Team
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		race, err := s.raceOf(ctx, tenantID, eventID, raceID)
		if err != nil {
			return err
		}
		if verr := ValidateTeam(name, category, race); verr != nil {
			return verr
		}
		now := time.Now().UTC()
		t := &Team{
			ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID, RaceID: raceID,
			Name: trimSpace(name), GenderCategory: category, Status: StatusRegistered, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.CreateTeam(ctx, t); err != nil {
			return err
		}
		team = t
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionTeamCreated,
			map[string]any{"team_id": t.ID, "event_id": eventID, "name": t.Name})
	})
	if err != nil {
		return nil, err
	}
	return team, nil
}

// UpdateTeam renames a team or changes its category; a category change is
// checked against the members it already has.
func (s *Service) UpdateTeam(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, id string, name *string, category *GenderCategory) (*Team, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return nil, err
	}
	var team *Team
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		t, err := s.repo.GetTeam(ctx, tenantID, eventID, id)
		if err != nil {
			return err
		}
		if name != nil {
			t.Name = *name
		}
		if category != nil {
			t.GenderCategory = *category
		}
		race, err := s.raceOf(ctx, tenantID, eventID, t.RaceID)
		if err != nil {
			return err
		}
		if verr := ValidateTeam(t.Name, t.GenderCategory, race); verr != nil {
			return verr
		}
		t.Name = trimSpace(t.Name)
		if category != nil {
			members, err := s.repo.ListTeamMembers(ctx, tenantID, []string{t.ID})
			if err != nil {
				return err
			}
			genders := make([]Gender, len(members))
			for i, m := range members {
				genders[i] = m.Gender
			}
			size := 0
			if race.TeamSize != nil {
				size = *race.TeamSize
			}
			if cerr, _ := CheckTeamComposition(t.GenderCategory, size, genders); cerr != nil {
				return cerr
			}
		}
		if err := s.repo.UpdateTeam(ctx, t); err != nil {
			return err
		}
		team = t
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionTeamUpdated,
			map[string]any{"team_id": t.ID, "event_id": eventID})
	})
	if err != nil {
		return nil, err
	}
	return team, nil
}

// DeleteTeam removes a team (Admin+). With withMembers its members are
// deleted too; otherwise they stay registered, without a team.
func (s *Service) DeleteTeam(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, id string, withMembers bool) error {
	if err := requireRole(actorRole, rbac.RoleAdmin); err != nil {
		return err
	}
	return s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if _, err := s.repo.GetTeam(ctx, tenantID, eventID, id); err != nil {
			return err
		}
		// participants.team_id cascades on delete, so keeping the members
		// means detaching them first.
		if !withMembers {
			if err := s.repo.DetachTeamMembers(ctx, tenantID, id); err != nil {
				return err
			}
		}
		if err := s.repo.DeleteTeam(ctx, tenantID, eventID, id); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionTeamDeleted,
			map[string]any{"team_id": id, "event_id": eventID, "with_members": withMembers})
	})
}
