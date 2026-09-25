package participant

import (
	"strings"
	"testing"
)

func TestListFilterWhere(t *testing.T) {
	t.Run("always scoped to tenant and event", func(t *testing.T) {
		clause, args := ListFilter{}.where("T", "E")
		if clause != "p.tenant_id = $1 AND p.event_id = $2" || len(args) != 2 || args[0] != "T" || args[1] != "E" {
			t.Errorf("clause = %q args = %v", clause, args)
		}
	})

	t.Run("each filter is a bound parameter", func(t *testing.T) {
		bib := 42
		clause, args := ListFilter{
			RaceID: "R", TeamID: "TM", Status: StatusDNS, Gender: GenderFemale, Club: "Lawu", BibNumber: &bib,
		}.where("T", "E")
		for _, want := range []string{"p.race_id = $3", "p.team_id = $4", "p.status = $5", "p.gender = $6", "p.club = $7", "p.bib_number = $8"} {
			if !strings.Contains(clause, want) {
				t.Errorf("clause %q missing %q", clause, want)
			}
		}
		if len(args) != 8 || args[7] != 42 {
			t.Errorf("args = %v", args)
		}
	})

	t.Run("no club beats club, unassigned needs no argument", func(t *testing.T) {
		clause, args := ListFilter{NoClub: true, Club: "ignored", Unassigned: true}.where("T", "E")
		if !strings.Contains(clause, "p.club IS NULL") || !strings.Contains(clause, "p.team_id IS NULL") {
			t.Errorf("clause = %q", clause)
		}
		if strings.Contains(clause, "p.club = ") || len(args) != 2 {
			t.Errorf("club value must not be bound when NoClub is set: %q %v", clause, args)
		}
	})

	t.Run("digits mean an exact BIB", func(t *testing.T) {
		clause, args := ListFilter{Query: " 123 "}.where("T", "E")
		if !strings.Contains(clause, "p.bib_number = $3") || strings.Contains(clause, "LIKE") || args[2] != 123 {
			t.Errorf("clause = %q args = %v", clause, args)
		}
	})

	t.Run("text searches name, bib name and email with one escaped pattern", func(t *testing.T) {
		clause, args := ListFilter{Query: "50%_off\\"}.where("T", "E")
		if got := args[2]; got != `%50\%\_off\\%` {
			t.Errorf("pattern = %v, want wildcards escaped", got)
		}
		if strings.Count(clause, "LIKE $3") != 3 || !strings.Contains(clause, "ESCAPE") || !strings.Contains(clause, nameExpr) {
			t.Errorf("clause = %q", clause)
		}
		if len(args) != 3 {
			t.Errorf("the pattern must be bound once and reused, args = %v", args)
		}
	})

	t.Run("user input never appears in the SQL text", func(t *testing.T) {
		evil := "x'; DROP TABLE participants; --"
		clause, _ := ListFilter{RaceID: evil, Club: evil, Query: evil}.where("T", "E")
		if strings.Contains(clause, "DROP") {
			t.Errorf("input leaked into SQL: %q", clause)
		}
	})
}

func TestTeamFilterWhere(t *testing.T) {
	clause, args := TeamFilter{RaceID: "R", GenderCategory: CategoryMixed, Query: "42"}.where("T", "E")
	for _, want := range []string{"t.race_id = $3", "t.gender_category = $4", "lower(t.name) LIKE $5", "p.bib_number::text = $6"} {
		if !strings.Contains(clause, want) {
			t.Errorf("clause %q missing %q", clause, want)
		}
	}
	if len(args) != 6 || args[5] != "42" {
		t.Errorf("args = %v", args)
	}

	clause, args = TeamFilter{Query: "lawu"}.where("T", "E")
	if strings.Contains(clause, "bib_number") || len(args) != 3 {
		t.Errorf("a non-numeric query must not add a BIB comparison: %q %v", clause, args)
	}
}
