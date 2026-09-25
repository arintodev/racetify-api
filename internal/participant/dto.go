package participant

import "time"

const timeLayout = time.RFC3339

type ParticipantDTO struct {
	ID       string  `json:"id"`
	RaceID   string  `json:"race_id"`
	TeamID   *string `json:"team_id"`
	LegOrder *int    `json:"leg_order"`
	TeamName *string `json:"team_name"`

	FirstName string  `json:"first_name"`
	LastName  *string `json:"last_name"`
	BibName   *string `json:"bib_name"`
	BibNumber int     `json:"bib_number"`
	Gender    string  `json:"gender"`
	RefID     *string `json:"ref_id"`

	Email                 *string `json:"email"`
	Club                  *string `json:"club"`
	EmergencyContactName  *string `json:"emergency_contact_name"`
	EmergencyContactPhone *string `json:"emergency_contact_phone"`
	BloodType             *string `json:"blood_type"`

	Status       string `json:"status"`
	GunTimeMS    *int64 `json:"gun_time_ms"`
	NetTimeMS    *int64 `json:"net_time_ms"`
	TotalLaps    *int   `json:"total_laps"`
	OverallRank  *int   `json:"overall_rank"`
	CategoryRank *int   `json:"category_rank"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func participantResponse(p *Participant) ParticipantDTO {
	return ParticipantDTO{
		ID: p.ID, RaceID: p.RaceID, TeamID: p.TeamID, LegOrder: p.LegOrder, TeamName: p.TeamName,
		FirstName: p.FirstName, LastName: p.LastName, BibName: p.BibName, BibNumber: p.BibNumber,
		Gender: string(p.Gender), RefID: p.RefID,
		Email: p.Email, Club: p.Club, EmergencyContactName: p.EmergencyContactName,
		EmergencyContactPhone: p.EmergencyContactPhone, BloodType: p.BloodType,
		Status: string(p.Status), GunTimeMS: p.GunTimeMS, NetTimeMS: p.NetTimeMS, TotalLaps: p.TotalLaps,
		OverallRank: p.OverallRank, CategoryRank: p.CategoryRank,
		CreatedAt: p.CreatedAt.Format(timeLayout), UpdatedAt: p.UpdatedAt.Format(timeLayout),
	}
}

// participantFields are the writable fields shared by create and (as
// pointers) update requests.
type participantFields struct {
	FirstName             string  `json:"first_name"`
	LastName              *string `json:"last_name"`
	BibName               *string `json:"bib_name"`
	BibNumber             int     `json:"bib_number"`
	Gender                string  `json:"gender"`
	RefID                 *string `json:"ref_id"`
	Email                 *string `json:"email"`
	Club                  *string `json:"club"`
	EmergencyContactName  *string `json:"emergency_contact_name"`
	EmergencyContactPhone *string `json:"emergency_contact_phone"`
	BloodType             *string `json:"blood_type"`
	Status                string  `json:"status"`
	TeamID                *string `json:"team_id"`
	LegOrder              *int    `json:"leg_order"`
}

func (f participantFields) toInput() Input {
	return Input{
		FirstName: f.FirstName, LastName: f.LastName, BibName: f.BibName, BibNumber: f.BibNumber,
		Gender: Gender(f.Gender), RefID: f.RefID, Email: f.Email, Club: f.Club,
		EmergencyContactName: f.EmergencyContactName, EmergencyContactPhone: f.EmergencyContactPhone,
		BloodType: f.BloodType, Status: Status(f.Status), TeamID: f.TeamID, LegOrder: f.LegOrder,
	}
}

type updateParticipantRequest struct {
	RaceID                *string `json:"race_id"`
	FirstName             *string `json:"first_name"`
	LastName              *string `json:"last_name"`
	BibName               *string `json:"bib_name"`
	BibNumber             *int    `json:"bib_number"`
	Gender                *string `json:"gender"`
	RefID                 *string `json:"ref_id"`
	Email                 *string `json:"email"`
	Club                  *string `json:"club"`
	EmergencyContactName  *string `json:"emergency_contact_name"`
	EmergencyContactPhone *string `json:"emergency_contact_phone"`
	BloodType             *string `json:"blood_type"`
	Status                *string `json:"status"`
	TeamID                *string `json:"team_id"`
	LegOrder              *int    `json:"leg_order"`
}

func (r updateParticipantRequest) toPatch() Patch {
	p := Patch{
		RaceID: r.RaceID, FirstName: r.FirstName, LastName: r.LastName, BibName: r.BibName, BibNumber: r.BibNumber,
		RefID: r.RefID, Email: r.Email, Club: r.Club, EmergencyContactName: r.EmergencyContactName,
		EmergencyContactPhone: r.EmergencyContactPhone, BloodType: r.BloodType, TeamID: r.TeamID, LegOrder: r.LegOrder,
	}
	if r.Gender != nil {
		g := Gender(*r.Gender)
		p.Gender = &g
	}
	if r.Status != nil {
		s := Status(*r.Status)
		p.Status = &s
	}
	return p
}

// ---- summary ----

type summaryDTO struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
	ByGender map[string]int `json:"by_gender"`
	ByRace   map[string]int `json:"by_race"`
	// TeamsByRace counts teams per (team) race.
	TeamsByRace map[string]int `json:"teams_by_race"`
	Clubs       []string       `json:"clubs"`
}

func summaryResponse(s *Summary) summaryDTO {
	byStatus := make(map[string]int, len(s.ByStatus))
	for k, v := range s.ByStatus {
		byStatus[string(k)] = v
	}
	clubs := s.Clubs
	if clubs == nil {
		clubs = []string{}
	}
	return summaryDTO{Total: s.Total, ByStatus: byStatus, ByGender: s.ByGender, ByRace: s.ByRace, TeamsByRace: s.TeamsByRace, Clubs: clubs}
}

// ---- teams ----

type TeamMemberDTO struct {
	ID        string  `json:"id"`
	BibNumber int     `json:"bib_number"`
	FirstName string  `json:"first_name"`
	LastName  *string `json:"last_name"`
	LegOrder  *int    `json:"leg_order"`
	Gender    string  `json:"gender"`
}

type TeamDTO struct {
	ID             string          `json:"id"`
	RaceID         string          `json:"race_id"`
	Name           string          `json:"name"`
	GenderCategory string          `json:"gender_category"`
	Status         string          `json:"status"`
	GunTimeMS      *int64          `json:"gun_time_ms"`
	NetTimeMS      *int64          `json:"net_time_ms"`
	TotalLaps      *int            `json:"total_laps"`
	OverallRank    *int            `json:"overall_rank"`
	CategoryRank   *int            `json:"category_rank"`
	Members        []TeamMemberDTO `json:"members"`
	CreatedAt      string          `json:"created_at"`
}

func teamResponse(t *TeamWithMembers) TeamDTO {
	members := make([]TeamMemberDTO, len(t.Members))
	for i, m := range t.Members {
		members[i] = TeamMemberDTO{ID: m.ID, BibNumber: m.BibNumber, FirstName: m.FirstName, LastName: m.LastName, LegOrder: m.LegOrder, Gender: string(m.Gender)}
	}
	return TeamDTO{
		ID: t.ID, RaceID: t.RaceID, Name: t.Name, GenderCategory: string(t.GenderCategory), Status: string(t.Status),
		GunTimeMS: t.GunTimeMS, NetTimeMS: t.NetTimeMS, TotalLaps: t.TotalLaps,
		OverallRank: t.OverallRank, CategoryRank: t.CategoryRank, Members: members,
		CreatedAt: t.CreatedAt.Format(timeLayout),
	}
}

// ---- import ----

type importRowRequest struct {
	Line   int    `json:"line"`
	RaceID string `json:"race_id"`
	participantFields
	TeamName     string `json:"team_name"`
	TeamCategory string `json:"team_category"`
}

type importRequest struct {
	RaceID        string             `json:"race_id"`
	OnExistingBib string             `json:"on_existing_bib"` // "skip" | "update"
	DryRun        bool               `json:"dry_run"`
	Rows          []importRowRequest `json:"rows"`
}

func (r importRequest) toImportRequest() ImportRequest {
	rows := make([]ImportRow, len(r.Rows))
	for i, row := range r.Rows {
		in := row.participantFields.toInput()
		in.Status = StatusRegistered // an import registers people; results set the rest
		rows[i] = ImportRow{
			Line: row.Line, Input: in, RaceID: row.RaceID,
			TeamName: trimSpace(row.TeamName), TeamCategory: GenderCategory(row.TeamCategory),
		}
	}
	return ImportRequest{
		DefaultRaceID: r.RaceID, UpdateExisting: r.OnExistingBib == "update", DryRun: r.DryRun, Rows: rows,
	}
}

type issueDTO struct {
	Level   string `json:"level"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type importRowDTO struct {
	Line   int        `json:"line"`
	Bib    int        `json:"bib_number"`
	Name   string     `json:"name"`
	Action string     `json:"action"`
	Issues []issueDTO `json:"issues"`
}

type importResultDTO struct {
	DryRun   bool           `json:"dry_run"`
	Created  int            `json:"created"`
	Updated  int            `json:"updated"`
	Skipped  int            `json:"skipped"`
	Errors   int            `json:"errors"`
	NewTeams int            `json:"new_teams"`
	Rows     []importRowDTO `json:"rows"`
}

func importResponse(res *ImportResult) importResultDTO {
	rows := make([]importRowDTO, len(res.Plan.Rows))
	for i, r := range res.Plan.Rows {
		issues := make([]issueDTO, len(r.Issues))
		for j, is := range r.Issues {
			issues[j] = issueDTO{Level: string(is.Level), Field: is.Field, Message: is.Message}
		}
		rows[i] = importRowDTO{Line: r.Line, Bib: r.BibNumber, Name: r.Name, Action: string(r.Action), Issues: issues}
	}
	return importResultDTO{
		DryRun: res.DryRun, Created: res.Created, Updated: res.Updated, Skipped: res.Skipped,
		Errors: res.Errors, NewTeams: res.NewTeams, Rows: rows,
	}
}
