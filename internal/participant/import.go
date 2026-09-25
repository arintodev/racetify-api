package participant

import (
	"fmt"
	"strings"
)

// Bulk registration import. Parsing a spreadsheet and mapping its columns is
// the browser's job; what arrives here are already-typed rows. This file is
// the authority on whether each row is acceptable: PlanImport decides, from
// the rows and the event's current data, what every row would do - with no
// database access, so the rules are testable on their own. The service
// executes the plan (or, for a dry run, only reports it).

type IssueLevel string

const (
	IssueError   IssueLevel = "error"
	IssueWarning IssueLevel = "warning"
)

type Issue struct {
	Level   IssueLevel
	Field   string
	Message string
}

type RowAction string

const (
	ActionCreate RowAction = "create"
	ActionUpdate RowAction = "update"
	ActionSkip   RowAction = "skip"
	ActionError  RowAction = "error"
)

// ImportRow is one incoming row, already normalised (Input.Normalize).
type ImportRow struct {
	// Line is the row's line in the source file, for reporting only.
	Line  int
	Input Input
	// RaceID is the race the row registers into.
	RaceID       string
	TeamName     string
	TeamCategory GenderCategory
	// BadBloodType, when set, is a blood type the row carried that could
	// not be recognised; it was dropped and is reported as a warning.
	BadBloodType string
}

// ExistingParticipant is the part of a stored participant the planner needs.
type ExistingParticipant struct {
	ID        string
	RaceID    string
	BibNumber int
	RefID     *string
	TeamID    *string
	LegOrder  *int
	Gender    Gender
}

// ExistingTeam is a stored team with its current members.
type ExistingTeam struct {
	ID       string
	RaceID   string
	Name     string
	Category GenderCategory
	Members  []ExistingParticipant
}

// PlanInput is everything PlanImport needs to decide.
type PlanInput struct {
	Rows []ImportRow
	// OnExistingBib chooses what happens to a row that matches an existing
	// participant: "update" overwrites it, anything else skips it.
	UpdateExisting bool
	Races          map[string]RaceFormat
	// Existing holds every stored participant that could match a row, by BIB
	// or by ref_id (the caller loads just those).
	Existing []ExistingParticipant
	Teams    []ExistingTeam
}

// TeamRef says which team a planned row belongs in: an existing one, or one
// the import creates (identified by its key).
type TeamRef struct {
	ExistingID string
	NewKey     string
}

// PlannedTeam is a team the import will create.
type PlannedTeam struct {
	Key      string
	RaceID   string
	Name     string
	Category GenderCategory
}

// RowPlan is the decision for one row.
type RowPlan struct {
	Line       int
	BibNumber  int
	Name       string
	Action     RowAction
	Issues     []Issue
	ExistingID string
	Team       *TeamRef
}

type Plan struct {
	Rows     []RowPlan
	NewTeams []PlannedTeam
}

func (p *Plan) Counts() (create, update, skip, errs int) {
	for _, r := range p.Rows {
		switch r.Action {
		case ActionCreate:
			create++
		case ActionUpdate:
			update++
		case ActionSkip:
			skip++
		case ActionError:
			errs++
		}
	}
	return
}

func (r *RowPlan) hasError() bool {
	for _, i := range r.Issues {
		if i.Level == IssueError {
			return true
		}
	}
	return false
}

func (r *RowPlan) addError(field, msg string) {
	r.Issues = append(r.Issues, Issue{IssueError, field, msg})
}
func (r *RowPlan) addWarning(field, msg string) {
	r.Issues = append(r.Issues, Issue{IssueWarning, field, msg})
}

func teamKey(raceID, name string) string {
	return raceID + "|" + strings.ToLower(strings.TrimSpace(name))
}

// pendingTeam stands in for a real team id while a row is validated: the
// team is resolved by name afterwards, but the field rules only care that
// one is set.
const pendingTeam = "pending"

// PlanImport decides what each row does. Rows with an error are never
// applied; the rest are independent of them.
func PlanImport(in PlanInput) Plan {
	plan := Plan{Rows: make([]RowPlan, len(in.Rows))}

	byBib := map[int]ExistingParticipant{}
	byRef := map[string]ExistingParticipant{}
	for _, e := range in.Existing {
		byBib[e.BibNumber] = e
		if e.RefID != nil {
			byRef[*e.RefID] = e
		}
	}
	fileBibs := map[int]int{}
	fileRefs := map[string]int{}
	for _, r := range in.Rows {
		fileBibs[r.Input.BibNumber]++
		if r.Input.RefID != nil {
			fileRefs[*r.Input.RefID]++
		}
	}

	// ---- pass 1: each row on its own ----
	for i, row := range in.Rows {
		rp := &plan.Rows[i]
		rp.Line, rp.BibNumber = row.Line, row.Input.BibNumber
		rp.Name = fmtName(row.Input.FirstName, row.Input.LastName)

		if row.BadBloodType != "" {
			rp.addWarning("blood_type", fmt.Sprintf("blood type %q not recognised, ignored", row.BadBloodType))
		}
		race, ok := in.Races[row.RaceID]
		if !ok {
			rp.addError("race_id", "race not found in this event")
			rp.Action = ActionError
			continue
		}

		input := row.Input
		if race.IsTeam() {
			input.TeamID = ptr(pendingTeam)
			if row.TeamName == "" {
				rp.addError("team_name", "team name is required for a team race")
			}
		}
		if err := input.Validate(race); err != nil && !(race.IsTeam() && row.TeamName == "" && err.Field == "team_id") {
			rp.addError(err.Field, err.Message)
		}
		if row.Input.BibNumber > 0 && fileBibs[row.Input.BibNumber] > 1 {
			rp.addError("bib_number", fmt.Sprintf("BIB %d appears %d times in this file", row.Input.BibNumber, fileBibs[row.Input.BibNumber]))
		}
		if ref := row.Input.RefID; ref != nil && fileRefs[*ref] > 1 {
			rp.addError("ref_id", fmt.Sprintf("ref_id %q appears %d times in this file", *ref, fileRefs[*ref]))
		}
		if rp.hasError() {
			rp.Action = ActionError
			continue
		}

		// Match an existing participant: ref_id first, BIB otherwise.
		var match *ExistingParticipant
		if ref := row.Input.RefID; ref != nil {
			if e, found := byRef[*ref]; found {
				match = &e
				if e.BibNumber != row.Input.BibNumber {
					if other, taken := byBib[row.Input.BibNumber]; taken && other.ID != e.ID {
						rp.addError("bib_number", fmt.Sprintf("BIB %d already belongs to another participant", row.Input.BibNumber))
					} else {
						rp.addWarning("bib_number", fmt.Sprintf("BIB changes from %d to %d", e.BibNumber, row.Input.BibNumber))
					}
				}
			} else if other, taken := byBib[row.Input.BibNumber]; taken {
				if other.RefID != nil {
					rp.addError("bib_number", fmt.Sprintf("BIB %d belongs to another registration", row.Input.BibNumber))
				} else {
					match = &other // matched by BIB; the update fills in ref_id
				}
			}
		} else if other, taken := byBib[row.Input.BibNumber]; taken {
			match = &other
		}
		if rp.hasError() {
			rp.Action = ActionError
			continue
		}

		switch {
		case match == nil:
			rp.Action = ActionCreate
		case in.UpdateExisting:
			rp.Action, rp.ExistingID = ActionUpdate, match.ID
			rp.addWarning("bib_number", fmt.Sprintf("BIB %d already exists; it will be updated", match.BibNumber))
		default:
			rp.Action, rp.ExistingID = ActionSkip, match.ID
			rp.addWarning("bib_number", fmt.Sprintf("BIB %d already exists; skipped", match.BibNumber))
		}
	}

	planTeams(in, &plan)
	return plan
}

// planTeams checks each team across the rows that join it, and resolves
// every team row to an existing or a new team.
func planTeams(in PlanInput, plan *Plan) {
	teamsByKey := map[string]ExistingTeam{}
	for _, t := range in.Teams {
		teamsByKey[teamKey(t.RaceID, t.Name)] = t
	}

	// Rows joining each team, in file order. Skipped and failed rows do not
	// join: a skipped row leaves its existing membership as it was.
	groups := map[string][]int{}
	var order []string
	for i, row := range in.Rows {
		rp := &plan.Rows[i]
		if rp.Action != ActionCreate && rp.Action != ActionUpdate {
			continue
		}
		if race := in.Races[row.RaceID]; !race.IsTeam() || row.TeamName == "" {
			continue
		}
		key := teamKey(row.RaceID, row.TeamName)
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	for _, key := range order {
		idx := groups[key]
		first := in.Rows[idx[0]]
		race := in.Races[first.RaceID]
		size := 0
		if race.TeamSize != nil {
			size = *race.TeamSize
		}
		team, exists := teamsByKey[key]

		// Category: an existing team keeps its own; a new one needs it stated.
		category := team.Category
		if !exists {
			for _, i := range idx {
				if in.Rows[i].TeamCategory != "" {
					category = in.Rows[i].TeamCategory
					break
				}
			}
			if category == "" {
				for _, i := range idx {
					plan.Rows[i].addError("team_category", fmt.Sprintf("new team %q needs a category", first.TeamName))
				}
				continue
			}
			if !category.Valid() {
				for _, i := range idx {
					plan.Rows[i].addError("team_category", "team category must be male, female or mixed")
				}
				continue
			}
		}

		// Members that stay: existing ones this file does not re-register.
		importing := map[string]bool{}
		for _, i := range idx {
			if id := plan.Rows[i].ExistingID; id != "" {
				importing[id] = true
			}
		}
		var staying []ExistingParticipant
		for _, m := range team.Members {
			if !importing[m.ID] {
				staying = append(staying, m)
			}
		}

		usedLegs := map[int]bool{}
		for _, m := range staying {
			if m.LegOrder != nil {
				usedLegs[*m.LegOrder] = true
			}
		}
		capacity := size - len(staying)
		genders := make([]Gender, 0, size)
		for _, m := range staying {
			genders = append(genders, m.Gender)
		}

		var accepted []int
		for _, i := range idx {
			rp := &plan.Rows[i]
			if leg := in.Rows[i].Input.LegOrder; leg != nil {
				if usedLegs[*leg] {
					rp.addError("leg_order", fmt.Sprintf("team %q: leg %d is used twice", first.TeamName, *leg))
				}
				usedLegs[*leg] = true
			}
			if capacity <= 0 {
				rp.addError("team_name", fmt.Sprintf("team %q would exceed %d members", first.TeamName, size))
			}
			capacity--
			if !rp.hasError() {
				accepted = append(accepted, i)
				genders = append(genders, in.Rows[i].Input.Gender)
			}
		}
		if len(accepted) == 0 {
			continue
		}

		if cerr, warn := CheckTeamComposition(category, size, genders); cerr != nil {
			for _, i := range accepted {
				plan.Rows[i].addError(cerr.Field, fmt.Sprintf("team %q: %s", first.TeamName, cerr.Message))
			}
			continue
		} else if warn != "" {
			plan.Rows[accepted[0]].addWarning("gender", fmt.Sprintf("team %q: %s", first.TeamName, warn))
		}
		if total := len(staying) + len(accepted); total < size {
			plan.Rows[accepted[0]].addWarning("team_name", fmt.Sprintf("team %q has %d of %d members", first.TeamName, total, size))
		}

		ref := &TeamRef{}
		if exists {
			ref.ExistingID = team.ID
		} else {
			ref.NewKey = key
			plan.NewTeams = append(plan.NewTeams, PlannedTeam{Key: key, RaceID: first.RaceID, Name: strings.TrimSpace(first.TeamName), Category: category})
		}
		for _, i := range accepted {
			plan.Rows[i].Team = ref
		}
	}

	// A row that picked up an error above no longer applies, and a team
	// nobody could join is not created.
	used := map[string]bool{}
	for i := range plan.Rows {
		rp := &plan.Rows[i]
		if rp.hasError() {
			rp.Action, rp.Team = ActionError, nil
			continue
		}
		if rp.Team != nil && rp.Team.NewKey != "" {
			used[rp.Team.NewKey] = true
		}
	}
	kept := plan.NewTeams[:0]
	for _, t := range plan.NewTeams {
		if used[t.Key] {
			kept = append(kept, t)
		}
	}
	plan.NewTeams = kept
}
