package participant

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// Repository reads and writes participants and teams. Every method runs on
// the connection in ctx, which the service opens as a tenant-scoped
// transaction (WithTenantTx), so row-level security applies on top of the
// explicit tenant_id / event_id conditions used here.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// uniqueConstraint names the constraint a unique violation hit ("" if err is
// not one), so callers can tell a taken BIB from a taken leg.
func uniqueConstraint(err error) string {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return pqErr.Constraint
	}
	return ""
}

// ==================== races ====================

// ListRaceFormats returns the format of every race in an event.
func (r *Repository) ListRaceFormats(ctx context.Context, tenantID, eventID string) (map[string]RaceFormat, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT id, event_id, name, entry_type, course_type, team_size FROM races WHERE tenant_id = $1 AND event_id = $2`,
		tenantID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RaceFormat{}
	for rows.Next() {
		var f RaceFormat
		var size sql.NullInt64
		if err := rows.Scan(&f.ID, &f.EventID, &f.Name, &f.EntryType, &f.CourseType, &size); err != nil {
			return nil, err
		}
		if size.Valid {
			f.TeamSize = ptr(int(size.Int64))
		}
		out[f.ID] = f
	}
	return out, rows.Err()
}

// ==================== participants ====================

const participantColumns = `p.id, p.tenant_id, p.event_id, p.race_id, p.team_id, p.leg_order,
	p.first_name, p.last_name, p.bib_name, p.bib_number, p.gender, p.ref_id,
	p.email, p.club, p.emergency_contact_name, p.emergency_contact_phone, p.blood_type,
	p.status, p.gun_time_ms, p.net_time_ms, p.total_laps, p.overall_rank, p.category_rank,
	p.created_at, p.updated_at,
	(SELECT tm.name FROM teams tm WHERE tm.id = p.team_id)`

func scanParticipant(row dbutil.RowScanner) (*Participant, error) {
	p := &Participant{}
	var gender sql.NullString
	err := row.Scan(
		&p.ID, &p.TenantID, &p.EventID, &p.RaceID, &p.TeamID, &p.LegOrder,
		&p.FirstName, &p.LastName, &p.BibName, &p.BibNumber, &gender, &p.RefID,
		&p.Email, &p.Club, &p.EmergencyContactName, &p.EmergencyContactPhone, &p.BloodType,
		&p.Status, &p.GunTimeMS, &p.NetTimeMS, &p.TotalLaps, &p.OverallRank, &p.CategoryRank,
		&p.CreatedAt, &p.UpdatedAt, &p.TeamName,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	// Rows that predate migration 0012 may have no gender.
	p.Gender = Gender(gender.String)
	return p, nil
}

// translateWrite turns the unique violations a participant write can hit
// into errors the client can act on.
func translateWrite(err error) error {
	switch uniqueConstraint(err) {
	case "":
		return err
	case "participants_event_bib_uk":
		return conflict("bib_taken", "bib_number", "This BIB number is already used in this event.")
	case "participants_team_leg_uk":
		return conflict("leg_taken", "leg_order", "This leg is already taken in the team.")
	case "teams_race_name_uk":
		return conflict("team_name_taken", "name", "A team with this name already exists in this race.")
	default:
		if strings.Contains(err.Error(), "ref_id") {
			return conflict("ref_id_taken", "ref_id", "This ref_id is already used in this event.")
		}
		return domain.ErrAlreadyExists
	}
}

func (r *Repository) CreateParticipant(ctx context.Context, p *Participant) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO participants (id, tenant_id, event_id, race_id, team_id, leg_order,
			first_name, last_name, bib_name, bib_number, gender, ref_id,
			email, club, emergency_contact_name, emergency_contact_phone, blood_type, status,
			created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		p.ID, p.TenantID, p.EventID, p.RaceID, p.TeamID, p.LegOrder,
		p.FirstName, p.LastName, p.BibName, p.BibNumber, string(p.Gender), p.RefID,
		p.Email, p.Club, p.EmergencyContactName, p.EmergencyContactPhone, p.BloodType, string(p.Status),
		p.CreatedAt, p.UpdatedAt)
	return translateWrite(err)
}

func (r *Repository) GetParticipant(ctx context.Context, tenantID, eventID, id string) (*Participant, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+participantColumns+` FROM participants p WHERE p.tenant_id = $1 AND p.event_id = $2 AND p.id = $3`,
		tenantID, eventID, id)
	return scanParticipant(row)
}

// UpdateParticipant writes the registration fields. Result columns
// (times, laps, ranks) are deliberately not touched here.
func (r *Repository) UpdateParticipant(ctx context.Context, p *Participant) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE participants SET race_id = $4, team_id = $5, leg_order = $6,
			first_name = $7, last_name = $8, bib_name = $9, bib_number = $10, gender = $11, ref_id = $12,
			email = $13, club = $14, emergency_contact_name = $15, emergency_contact_phone = $16,
			blood_type = $17, status = $18
		WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		p.TenantID, p.EventID, p.ID, p.RaceID, p.TeamID, p.LegOrder,
		p.FirstName, p.LastName, p.BibName, p.BibNumber, string(p.Gender), p.RefID,
		p.Email, p.Club, p.EmergencyContactName, p.EmergencyContactPhone,
		p.BloodType, string(p.Status))
	if err != nil {
		return translateWrite(err)
	}
	return dbutil.CheckRowsAffected(res)
}

// ---- list (keyset-paginated by BIB) ----

func encodeBibCursor(bib int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(bib)))
}

func decodeBibCursor(raw string) (int, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(b))
	return n, err == nil
}

// ListParticipants returns one page of participants matching f, ordered by
// BIB. BIB is unique within an event, so it alone is a stable cursor.
func (r *Repository) ListParticipants(ctx context.Context, tenantID, eventID string, f ListFilter, page pagination.PageParams) (pagination.Page[Participant], error) {
	limit := page.NormalizeLimit()
	clause, args := f.where(tenantID, eventID)
	if bib, ok := decodeBibCursor(page.Cursor); ok {
		args = append(args, bib)
		clause += " AND p.bib_number > $" + strconv.Itoa(len(args))
	}
	query := `SELECT ` + participantColumns + ` FROM participants p WHERE ` + clause +
		` ORDER BY p.bib_number ASC LIMIT ` + strconv.Itoa(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[Participant]{}, err
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return pagination.Page[Participant]{}, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[Participant]{}, err
	}
	var next string
	if len(out) > limit {
		next = encodeBibCursor(out[limit-1].BibNumber)
		out = out[:limit]
	}
	return pagination.Page[Participant]{Items: out, NextCursor: next}, nil
}

// ForEachParticipant streams every participant matching f, in BIB order,
// to fn without holding them all in memory (used by export).
func (r *Repository) ForEachParticipant(ctx context.Context, tenantID, eventID string, f ListFilter, fn func(*Participant) error) error {
	clause, args := f.where(tenantID, eventID)
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT `+participantColumns+` FROM participants p WHERE `+clause+` ORDER BY p.bib_number ASC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return rows.Err()
}

// idsClause narrows a where-clause (built by ListFilter.where) to specific
// ids, appending the id list as a bound parameter.
func idsClause(clause string, args []any, ids []string) (string, []any) {
	args = append(args, pq.Array(ids))
	return clause + " AND p.id = ANY($" + strconv.Itoa(len(args)) + "::uuid[])", args
}

// DeleteParticipants deletes everything matching the clause and returns how
// many rows went.
func (r *Repository) DeleteParticipants(ctx context.Context, clause string, args []any) (int64, error) {
	res, err := r.db.Q(ctx).ExecContext(ctx, `DELETE FROM participants AS p WHERE `+clause, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetParticipantsStatus sets the status of everything matching the clause.
func (r *Repository) SetParticipantsStatus(ctx context.Context, clause string, args []any, status Status) (int64, error) {
	args = append(args, string(status))
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE participants AS p SET status = $`+strconv.Itoa(len(args))+` WHERE `+clause, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- summary ----

type SummaryCounts struct {
	RaceID string
	Status Status
	Gender string
	Count  int
}

func (r *Repository) SummaryCounts(ctx context.Context, tenantID, eventID, raceID string) ([]SummaryCounts, error) {
	clause, args := ListFilter{RaceID: raceID}.where(tenantID, eventID)
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT p.race_id, p.status, coalesce(p.gender, ''), count(*) FROM participants p WHERE `+clause+
			` GROUP BY p.race_id, p.status, p.gender`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SummaryCounts
	for rows.Next() {
		var c SummaryCounts
		if err := rows.Scan(&c.RaceID, &c.Status, &c.Gender, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TeamCounts returns how many teams each race has.
func (r *Repository) TeamCounts(ctx context.Context, tenantID, eventID string) (map[string]int, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT race_id, count(*) FROM teams WHERE tenant_id = $1 AND event_id = $2 GROUP BY race_id`, tenantID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var raceID string
		var n int
		if err := rows.Scan(&raceID, &n); err != nil {
			return nil, err
		}
		out[raceID] = n
	}
	return out, rows.Err()
}

func (r *Repository) ListClubs(ctx context.Context, tenantID, eventID, raceID string) ([]string, error) {
	clause, args := ListFilter{RaceID: raceID}.where(tenantID, eventID)
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT DISTINCT p.club FROM participants p WHERE `+clause+` AND p.club IS NOT NULL ORDER BY p.club`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ==================== teams ====================

const teamColumns = `t.id, t.tenant_id, t.event_id, t.race_id, t.name, t.gender_category, t.status,
	t.gun_time_ms, t.net_time_ms, t.total_laps, t.overall_rank, t.category_rank, t.created_at, t.updated_at`

func scanTeam(row dbutil.RowScanner) (*Team, error) {
	t := &Team{}
	err := row.Scan(&t.ID, &t.TenantID, &t.EventID, &t.RaceID, &t.Name, &t.GenderCategory, &t.Status,
		&t.GunTimeMS, &t.NetTimeMS, &t.TotalLaps, &t.OverallRank, &t.CategoryRank, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return t, nil
}

func (r *Repository) CreateTeam(ctx context.Context, t *Team) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO teams (id, tenant_id, event_id, race_id, name, gender_category, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		t.ID, t.TenantID, t.EventID, t.RaceID, t.Name, string(t.GenderCategory), string(t.Status), t.CreatedAt, t.UpdatedAt)
	return translateWrite(err)
}

func (r *Repository) GetTeam(ctx context.Context, tenantID, eventID, id string) (*Team, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+teamColumns+` FROM teams t WHERE t.tenant_id = $1 AND t.event_id = $2 AND t.id = $3`,
		tenantID, eventID, id)
	return scanTeam(row)
}

func (r *Repository) UpdateTeam(ctx context.Context, t *Team) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE teams SET name = $4, gender_category = $5 WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		t.TenantID, t.EventID, t.ID, t.Name, string(t.GenderCategory))
	if err != nil {
		return translateWrite(err)
	}
	return dbutil.CheckRowsAffected(res)
}

// DetachTeamMembers takes every member out of a team (they stay registered,
// as unassigned participants).
func (r *Repository) DetachTeamMembers(ctx context.Context, tenantID, teamID string) error {
	_, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE participants SET team_id = NULL, leg_order = NULL WHERE tenant_id = $1 AND team_id = $2`, tenantID, teamID)
	return err
}

// DeleteTeam removes the team row. participants.team_id cascades, so callers
// that want to keep the members must DetachTeamMembers first.
func (r *Repository) DeleteTeam(ctx context.Context, tenantID, eventID, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`DELETE FROM teams WHERE tenant_id = $1 AND event_id = $2 AND id = $3`, tenantID, eventID, id)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// ListTeams returns one page of teams matching f, ordered by name.
func (r *Repository) ListTeams(ctx context.Context, tenantID, eventID string, f TeamFilter, page pagination.PageParams) (pagination.Page[Team], error) {
	limit := page.NormalizeLimit()
	clause, args := f.where(tenantID, eventID)
	if name, id, ok := decodeTeamCursor(page.Cursor); ok {
		args = append(args, name, id)
		clause += fmt.Sprintf(" AND (t.name, t.id::text) > ($%d, $%d)", len(args)-1, len(args))
	}
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT `+teamColumns+` FROM teams t WHERE `+clause+
			` ORDER BY t.name ASC, t.id::text ASC LIMIT `+strconv.Itoa(limit+1), args...)
	if err != nil {
		return pagination.Page[Team]{}, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return pagination.Page[Team]{}, err
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[Team]{}, err
	}
	var next string
	if len(out) > limit {
		next = encodeTeamCursor(out[limit-1].Name, out[limit-1].ID)
		out = out[:limit]
	}
	return pagination.Page[Team]{Items: out, NextCursor: next}, nil
}

func encodeTeamCursor(name, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id + "|" + name))
}

func decodeTeamCursor(raw string) (name, id string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", "", false
	}
	id, name, ok = strings.Cut(string(b), "|")
	return name, id, ok && id != ""
}

// TeamMember is the slim view of a team member shown inside a team.
type TeamMember struct {
	ID        string
	TeamID    string
	BibNumber int
	FirstName string
	LastName  *string
	LegOrder  *int
	Gender    Gender
}

// ListTeamMembers returns the members of the given teams, ordered by leg
// then BIB.
func (r *Repository) ListTeamMembers(ctx context.Context, tenantID string, teamIDs []string) ([]TeamMember, error) {
	if len(teamIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Q(ctx).QueryContext(ctx, `
		SELECT id, team_id, bib_number, first_name, last_name, leg_order, coalesce(gender, '')
		FROM participants WHERE tenant_id = $1 AND team_id = ANY($2::uuid[])
		ORDER BY leg_order NULLS LAST, bib_number`, tenantID, pq.Array(teamIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamMember
	for rows.Next() {
		var m TeamMember
		var g string
		if err := rows.Scan(&m.ID, &m.TeamID, &m.BibNumber, &m.FirstName, &m.LastName, &m.LegOrder, &g); err != nil {
			return nil, err
		}
		m.Gender = Gender(g)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ==================== import support ====================

// FindForImport returns the participants of an event that any incoming row
// could match, by BIB or by ref_id.
func (r *Repository) FindForImport(ctx context.Context, tenantID, eventID string, bibs []int64, refs []string) ([]ExistingParticipant, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx, `
		SELECT id, race_id, bib_number, ref_id, team_id, leg_order, coalesce(gender, '')
		FROM participants
		WHERE tenant_id = $1 AND event_id = $2 AND (bib_number = ANY($3) OR ref_id = ANY($4))`,
		tenantID, eventID, pq.Array(bibs), pq.Array(refs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExistingParticipant
	for rows.Next() {
		var e ExistingParticipant
		var g string
		if err := rows.Scan(&e.ID, &e.RaceID, &e.BibNumber, &e.RefID, &e.TeamID, &e.LegOrder, &g); err != nil {
			return nil, err
		}
		e.Gender = Gender(g)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAllTeams returns every team of an event (unpaginated: the import
// planner needs them all to match by name) with its members.
func (r *Repository) ListAllTeams(ctx context.Context, tenantID, eventID string) ([]ExistingTeam, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT id, race_id, name, gender_category FROM teams WHERE tenant_id = $1 AND event_id = $2`, tenantID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var teams []ExistingTeam
	var ids []string
	for rows.Next() {
		var t ExistingTeam
		if err := rows.Scan(&t.ID, &t.RaceID, &t.Name, &t.Category); err != nil {
			return nil, err
		}
		teams = append(teams, t)
		ids = append(ids, t.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	members, err := r.ListTeamMembers(ctx, tenantID, ids)
	if err != nil {
		return nil, err
	}
	byTeam := map[string][]ExistingParticipant{}
	for _, m := range members {
		byTeam[m.TeamID] = append(byTeam[m.TeamID], ExistingParticipant{
			ID: m.ID, BibNumber: m.BibNumber, LegOrder: m.LegOrder, Gender: m.Gender, TeamID: ptr(m.TeamID),
		})
	}
	for i := range teams {
		teams[i].Members = byTeam[teams[i].ID]
	}
	return teams, nil
}
