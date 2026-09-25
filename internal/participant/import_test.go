package participant

import (
	"strings"
	"testing"
)

func row(line int, bib int, first string, mod func(*ImportRow)) ImportRow {
	r := ImportRow{
		Line:   line,
		RaceID: "r1",
		Input:  Input{FirstName: first, BibNumber: bib, Gender: GenderMale, Status: StatusRegistered},
	}
	if mod != nil {
		mod(&r)
	}
	return r
}

func hasIssue(p RowPlan, level IssueLevel, contains string) bool {
	for _, i := range p.Issues {
		if i.Level == level && strings.Contains(i.Message, contains) {
			return true
		}
	}
	return false
}

var individualRaces = map[string]RaceFormat{"r1": {ID: "r1", EntryType: "individual", CourseType: "standard"}}

func TestPlanImportMatching(t *testing.T) {
	existing := []ExistingParticipant{
		{ID: "p-100", RaceID: "r1", BibNumber: 100},                      // no ref_id
		{ID: "p-200", RaceID: "r1", BibNumber: 200, RefID: ptr("INV-2")}, // has ref_id
		{ID: "p-300", RaceID: "r1", BibNumber: 300, RefID: ptr("INV-3")},
	}
	tests := []struct {
		name       string
		row        ImportRow
		update     bool
		wantAction RowAction
		wantID     string
		wantIssue  string
		wantLevel  IssueLevel
	}{
		{"new BIB creates", row(2, 1, "A", nil), false, ActionCreate, "", "", ""},
		{"existing BIB skips by default", row(2, 100, "A", nil), false, ActionSkip, "p-100", "already exists", IssueWarning},
		{"existing BIB updates when asked", row(2, 100, "A", nil), true, ActionUpdate, "p-100", "will be updated", IssueWarning},
		{"ref_id matches, same BIB", row(2, 200, "A", func(r *ImportRow) { r.Input.RefID = ptr("INV-2") }), true, ActionUpdate, "p-200", "", ""},
		{"ref_id matches, BIB changed to a free one", row(2, 201, "A", func(r *ImportRow) { r.Input.RefID = ptr("INV-2") }), true, ActionUpdate, "p-200", "BIB changes from 200 to 201", IssueWarning},
		{"ref_id matches, new BIB taken by someone else", row(2, 300, "A", func(r *ImportRow) { r.Input.RefID = ptr("INV-2") }), true, ActionError, "", "already belongs to another participant", IssueError},
		{"new ref_id, BIB free", row(2, 5, "A", func(r *ImportRow) { r.Input.RefID = ptr("NEW") }), false, ActionCreate, "", "", ""},
		{"new ref_id, BIB held by another registration", row(2, 300, "A", func(r *ImportRow) { r.Input.RefID = ptr("NEW") }), true, ActionError, "", "belongs to another registration", IssueError},
		{"new ref_id, BIB held by a participant without ref_id", row(2, 100, "A", func(r *ImportRow) { r.Input.RefID = ptr("NEW") }), true, ActionUpdate, "p-100", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := PlanImport(PlanInput{Rows: []ImportRow{tt.row}, UpdateExisting: tt.update, Races: individualRaces, Existing: existing})
			got := plan.Rows[0]
			if got.Action != tt.wantAction || got.ExistingID != tt.wantID {
				t.Fatalf("action = %s id = %q, want %s %q (issues %v)", got.Action, got.ExistingID, tt.wantAction, tt.wantID, got.Issues)
			}
			if tt.wantIssue != "" && !hasIssue(got, tt.wantLevel, tt.wantIssue) {
				t.Errorf("want %s issue containing %q, got %v", tt.wantLevel, tt.wantIssue, got.Issues)
			}
		})
	}
}

func TestPlanImportRowValidation(t *testing.T) {
	plan := PlanImport(PlanInput{
		Races: individualRaces,
		Rows: []ImportRow{
			row(2, 1, "", nil), // no first name
			row(3, 2, "B", func(r *ImportRow) { r.RaceID = "nope" }),
			row(4, 5, "C", nil),
			row(5, 5, "D", nil), // duplicate BIB in the file
			row(6, 6, "E", func(r *ImportRow) { r.BadBloodType = "zzz" }),
		},
	})
	want := []RowAction{ActionError, ActionError, ActionError, ActionError, ActionCreate}
	for i, w := range want {
		if plan.Rows[i].Action != w {
			t.Errorf("row %d action = %s, want %s (%v)", i, plan.Rows[i].Action, w, plan.Rows[i].Issues)
		}
	}
	if !hasIssue(plan.Rows[2], IssueError, "appears 2 times") || !hasIssue(plan.Rows[3], IssueError, "appears 2 times") {
		t.Errorf("both copies of a duplicated BIB must be flagged: %v %v", plan.Rows[2].Issues, plan.Rows[3].Issues)
	}
	if !hasIssue(plan.Rows[4], IssueWarning, "not recognised") {
		t.Errorf("unrecognised blood type should warn, got %v", plan.Rows[4].Issues)
	}
}

func teamRow(line, bib int, gender Gender, team string, cat GenderCategory, leg *int) ImportRow {
	return row(line, bib, "P"+team, func(r *ImportRow) {
		r.RaceID = "team"
		r.Input.Gender = gender
		r.Input.LegOrder = leg
		r.TeamName = team
		r.TeamCategory = cat
	})
}

func TestPlanImportTeams(t *testing.T) {
	relay := map[string]RaceFormat{"team": {ID: "team", EntryType: "team", CourseType: "relay", TeamSize: ptr(2)}}
	loop := map[string]RaceFormat{"team": {ID: "team", EntryType: "team", CourseType: "loop", TeamSize: ptr(2)}}

	t.Run("a complete new team is created once", func(t *testing.T) {
		p := PlanImport(PlanInput{Races: relay, Rows: []ImportRow{
			teamRow(2, 1, GenderMale, "Lawu", CategoryMale, ptr(1)),
			teamRow(3, 2, GenderMale, "Lawu", "", ptr(2)),
		}})
		if len(p.NewTeams) != 1 || p.NewTeams[0].Name != "Lawu" || p.NewTeams[0].Category != CategoryMale {
			t.Fatalf("new teams = %+v", p.NewTeams)
		}
		for _, r := range p.Rows {
			if r.Action != ActionCreate || r.Team == nil || r.Team.NewKey == "" {
				t.Errorf("row %d: %s team %+v issues %v", r.Line, r.Action, r.Team, r.Issues)
			}
		}
	})

	t.Run("a new team needs a category", func(t *testing.T) {
		p := PlanImport(PlanInput{Races: relay, Rows: []ImportRow{teamRow(2, 1, GenderMale, "Lawu", "", ptr(1))}})
		if p.Rows[0].Action != ActionError || !hasIssue(p.Rows[0], IssueError, "needs a category") || len(p.NewTeams) != 0 {
			t.Errorf("got %s %v teams %v", p.Rows[0].Action, p.Rows[0].Issues, p.NewTeams)
		}
	})

	t.Run("team over capacity and duplicate leg", func(t *testing.T) {
		p := PlanImport(PlanInput{Races: relay, Rows: []ImportRow{
			teamRow(2, 1, GenderMale, "A", CategoryMale, ptr(1)),
			teamRow(3, 2, GenderMale, "A", CategoryMale, ptr(1)), // leg clash
			teamRow(4, 3, GenderMale, "A", CategoryMale, ptr(2)), // third member of a 2-person team
		}})
		if !hasIssue(p.Rows[1], IssueError, "leg 1 is used twice") {
			t.Errorf("leg clash not reported: %v", p.Rows[1].Issues)
		}
		if !hasIssue(p.Rows[2], IssueError, "exceed 2 members") {
			t.Errorf("capacity not reported: %v", p.Rows[2].Issues)
		}
		if p.Rows[0].Action != ActionCreate {
			t.Errorf("the valid member must still import: %s %v", p.Rows[0].Action, p.Rows[0].Issues)
		}
	})

	t.Run("existing members count against capacity and legs", func(t *testing.T) {
		existing := []ExistingTeam{{
			ID: "t1", RaceID: "team", Name: "Lawu", Category: CategoryMale,
			Members: []ExistingParticipant{{ID: "m1", BibNumber: 50, LegOrder: ptr(1), Gender: GenderMale}},
		}}
		p := PlanImport(PlanInput{Races: relay, Teams: existing, Rows: []ImportRow{
			teamRow(2, 1, GenderMale, "lawu", "", ptr(1)), // leg 1 already used by an existing member (case-insensitive team name)
		}})
		if !hasIssue(p.Rows[0], IssueError, "leg 1 is used twice") {
			t.Errorf("existing member's leg not considered: %v", p.Rows[0].Issues)
		}
		p = PlanImport(PlanInput{Races: relay, Teams: existing, Rows: []ImportRow{
			teamRow(2, 1, GenderMale, "LAWU", "", ptr(2)),
		}})
		if p.Rows[0].Action != ActionCreate || p.Rows[0].Team == nil || p.Rows[0].Team.ExistingID != "t1" || len(p.NewTeams) != 0 {
			t.Errorf("should join the existing team: %s %+v teams %v", p.Rows[0].Action, p.Rows[0].Team, p.NewTeams)
		}
	})

	t.Run("gender category is enforced", func(t *testing.T) {
		p := PlanImport(PlanInput{Races: loop, Rows: []ImportRow{
			teamRow(2, 1, GenderMale, "A", CategoryFemale, nil),
		}})
		if p.Rows[0].Action != ActionError || !hasIssue(p.Rows[0], IssueError, "female team") {
			t.Errorf("got %s %v", p.Rows[0].Action, p.Rows[0].Issues)
		}
	})

	t.Run("an incomplete team warns", func(t *testing.T) {
		p := PlanImport(PlanInput{Races: loop, Rows: []ImportRow{teamRow(2, 1, GenderMale, "A", CategoryMale, nil)}})
		if p.Rows[0].Action != ActionCreate || !hasIssue(p.Rows[0], IssueWarning, "1 of 2 members") {
			t.Errorf("got %s %v", p.Rows[0].Action, p.Rows[0].Issues)
		}
	})

	t.Run("a team whose members all failed is not created", func(t *testing.T) {
		p := PlanImport(PlanInput{Races: loop, Rows: []ImportRow{
			teamRow(2, 1, GenderMale, "A", CategoryFemale, nil),
		}})
		if len(p.NewTeams) != 0 {
			t.Errorf("no team should be created, got %+v", p.NewTeams)
		}
	})

	t.Run("a team race row without a team name is an error", func(t *testing.T) {
		p := PlanImport(PlanInput{Races: loop, Rows: []ImportRow{teamRow(2, 1, GenderMale, "", CategoryMale, nil)}})
		if p.Rows[0].Action != ActionError || !hasIssue(p.Rows[0], IssueError, "team name is required") {
			t.Errorf("got %s %v", p.Rows[0].Action, p.Rows[0].Issues)
		}
	})
}

func TestPlanCounts(t *testing.T) {
	p := PlanImport(PlanInput{Races: individualRaces, Existing: []ExistingParticipant{{ID: "x", BibNumber: 9}},
		Rows: []ImportRow{row(2, 1, "A", nil), row(3, 9, "B", nil), row(4, 2, "", nil)}})
	c, u, s, e := p.Counts()
	if c != 1 || u != 0 || s != 1 || e != 1 {
		t.Errorf("counts = create %d update %d skip %d error %d", c, u, s, e)
	}
}
