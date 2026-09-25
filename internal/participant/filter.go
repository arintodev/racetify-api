package participant

import (
	"fmt"
	"strconv"
	"strings"
)

// ListFilter is what GET /events/{id}/participants can narrow by. It
// mirrors the filters the dashboard offers, so the screen can ask the
// server instead of loading every row.
type ListFilter struct {
	RaceID string
	TeamID string
	Status Status
	Gender Gender
	// Club matches exactly; NoClub matches rows with no club at all.
	Club   string
	NoClub bool
	// Unassigned matches people not in any team.
	Unassigned bool
	BibNumber  *int
	// Query is free text: all digits means "this exact BIB", anything else
	// is a substring match on name, BIB name, and email.
	Query string
}

// escapeLike neutralises LIKE wildcards in user input (used with
// ESCAPE '\').
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// nameExpr must stay identical to the expression the name-search index in
// migrations/0012_team_loop_formats is built on, or the index goes unused.
const nameExpr = `lower(p.first_name || ' ' || coalesce(p.last_name, ''))`

// where builds the WHERE clause for the filter, with every value bound as
// a parameter (nothing from the request is ever spliced into the SQL). The
// clause always starts with tenant and event, so it can be used as-is
// anywhere participants are read.
func (f ListFilter) where(tenantID, eventID string) (clause string, args []any) {
	args = []any{tenantID, eventID}
	conds := []string{"p.tenant_id = $1", "p.event_id = $2"}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.Replace(cond, "?", "$"+strconv.Itoa(len(args)), 1))
	}

	if f.RaceID != "" {
		add("p.race_id = ?", f.RaceID)
	}
	if f.TeamID != "" {
		add("p.team_id = ?", f.TeamID)
	}
	if f.Unassigned {
		conds = append(conds, "p.team_id IS NULL")
	}
	if f.Status != "" {
		add("p.status = ?", string(f.Status))
	}
	if f.Gender != "" {
		add("p.gender = ?", string(f.Gender))
	}
	switch {
	case f.NoClub:
		conds = append(conds, "p.club IS NULL")
	case f.Club != "":
		add("p.club = ?", f.Club)
	}
	if f.BibNumber != nil {
		add("p.bib_number = ?", *f.BibNumber)
	}

	if q := strings.TrimSpace(f.Query); q != "" {
		if n, err := strconv.Atoi(q); err == nil && isDigits(q) {
			add("p.bib_number = ?", n)
		} else {
			args = append(args, "%"+escapeLike(strings.ToLower(q))+"%")
			ph := "$" + strconv.Itoa(len(args))
			conds = append(conds, fmt.Sprintf(
				"(%s LIKE %s ESCAPE '\\' OR lower(coalesce(p.bib_name, '')) LIKE %s ESCAPE '\\' OR lower(coalesce(p.email, '')) LIKE %s ESCAPE '\\')",
				nameExpr, ph, ph, ph))
		}
	}
	return strings.Join(conds, " AND "), args
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// TeamFilter is what GET /events/{id}/teams can narrow by.
type TeamFilter struct {
	RaceID         string
	GenderCategory GenderCategory
	// Query matches the team name, or the name / BIB of any member.
	Query string
}

func (f TeamFilter) where(tenantID, eventID string) (clause string, args []any) {
	args = []any{tenantID, eventID}
	conds := []string{"t.tenant_id = $1", "t.event_id = $2"}
	if f.RaceID != "" {
		args = append(args, f.RaceID)
		conds = append(conds, "t.race_id = $"+strconv.Itoa(len(args)))
	}
	if f.GenderCategory != "" {
		args = append(args, string(f.GenderCategory))
		conds = append(conds, "t.gender_category = $"+strconv.Itoa(len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+escapeLike(strings.ToLower(q))+"%")
		like := "$" + strconv.Itoa(len(args))
		member := fmt.Sprintf(
			"EXISTS (SELECT 1 FROM participants p WHERE p.team_id = t.id AND (%s LIKE %s ESCAPE '\\' OR lower(coalesce(p.bib_name, '')) LIKE %s ESCAPE '\\'",
			nameExpr, like, like)
		if isDigits(q) {
			args = append(args, q)
			member += " OR p.bib_number::text = $" + strconv.Itoa(len(args))
		}
		member += "))"
		conds = append(conds, fmt.Sprintf("(lower(t.name) LIKE %s ESCAPE '\\' OR %s)", like, member))
	}
	return strings.Join(conds, " AND "), args
}
