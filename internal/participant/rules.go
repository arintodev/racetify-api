package participant

import (
	"fmt"
	"strings"
)

// Input is the writable state of a participant: what a create supplies and
// what an update leaves after merging its patch onto the stored row.
type Input struct {
	FirstName             string
	LastName              *string
	BibName               *string
	BibNumber             int
	Gender                Gender
	RefID                 *string
	Email                 *string
	Club                  *string
	EmergencyContactName  *string
	EmergencyContactPhone *string
	BloodType             *string
	Status                Status
	TeamID                *string
	LegOrder              *int
}

// trimPtr trims a string pointer and turns blank into nil, so "" and
// missing mean the same thing everywhere.
func trimPtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := strings.TrimSpace(*p)
	if s == "" {
		return nil
	}
	return &s
}

// Normalize trims text fields, collapses blanks to nil, and canonicalises
// the blood type. It reports a blood type it cannot recognise so callers
// can choose between rejecting it (manual entry) and dropping it with a
// warning (import).
func (in *Input) Normalize() (badBloodType string) {
	in.FirstName = strings.TrimSpace(in.FirstName)
	in.LastName = trimPtr(in.LastName)
	in.BibName = trimPtr(in.BibName)
	in.RefID = trimPtr(in.RefID)
	in.Email = trimPtr(in.Email)
	in.Club = trimPtr(in.Club)
	in.EmergencyContactName = trimPtr(in.EmergencyContactName)
	in.EmergencyContactPhone = trimPtr(in.EmergencyContactPhone)
	in.TeamID = trimPtr(in.TeamID)
	if in.Status == "" {
		in.Status = StatusRegistered
	}
	if bt := trimPtr(in.BloodType); bt == nil {
		in.BloodType = nil
	} else if norm, ok := NormalizeBloodType(*bt); ok {
		in.BloodType = &norm
	} else {
		in.BloodType = nil
		return *bt
	}
	return ""
}

func validEmail(s string) bool {
	at := strings.Index(s, "@")
	return at > 0 && at < len(s)-1 && strings.Contains(s[at:], ".") && !strings.ContainsAny(s, " \t\r\n") && len(s) <= 254
}

// Validate checks a normalised Input against the race it belongs to. It is
// the single place the participant field rules live; team-level rules
// (capacity, leg clashes, gender category) need other rows and are checked
// separately.
func (in *Input) Validate(race RaceFormat) *Error {
	if in.FirstName == "" {
		return invalid("first_name", "first_name is required.")
	}
	if in.BibNumber <= 0 {
		return invalid("bib_number", "bib_number must be a positive number.")
	}
	if !in.Gender.Valid() {
		return invalid("gender", "gender must be male or female.")
	}
	if !in.Status.Valid() {
		return invalid("status", "status is not valid.")
	}
	if in.Email != nil && !validEmail(*in.Email) {
		return invalid("email", "email is not a valid address.")
	}

	if race.IsTeam() {
		if in.TeamID == nil {
			return invalid("team_id", "team_id is required for a team race.")
		}
	} else if in.TeamID != nil {
		return invalid("team_id", "team_id must be empty for an individual race.")
	}

	if race.IsRelay() {
		size := 0
		if race.TeamSize != nil {
			size = *race.TeamSize
		}
		if in.LegOrder == nil {
			return invalid("leg_order", "leg_order is required for a relay race.")
		}
		if *in.LegOrder < 1 || *in.LegOrder > size {
			return invalid("leg_order", fmt.Sprintf("leg_order must be between 1 and %d.", size))
		}
	} else if in.LegOrder != nil {
		return invalid("leg_order", "leg_order is only used in relay races.")
	}
	return nil
}

// CheckTeamComposition applies a team's gender category to its members'
// genders. Male and female teams require every member to match. A mixed
// team needs at least one of each, but that can only be judged once the
// team is complete: while it is still filling up, the absence of one gender
// is reported as a warning, not an error.
func CheckTeamComposition(category GenderCategory, teamSize int, genders []Gender) (err *Error, warning string) {
	switch category {
	case CategoryMale, CategoryFemale:
		want := Gender(category)
		for _, g := range genders {
			if g != want {
				return invalid("gender", fmt.Sprintf("a %s team can only have %s members.", category, want)), ""
			}
		}
	case CategoryMixed:
		var male, female bool
		for _, g := range genders {
			male = male || g == GenderMale
			female = female || g == GenderFemale
		}
		if !(male && female) {
			if teamSize > 0 && len(genders) >= teamSize {
				return invalid("gender", "a mixed team needs at least one male and one female member."), ""
			}
			return nil, "a mixed team needs at least one male and one female member."
		}
	}
	return nil, ""
}

// ValidateTeam checks a team's own fields.
func ValidateTeam(name string, category GenderCategory, race RaceFormat) *Error {
	if strings.TrimSpace(name) == "" {
		return invalid("name", "name is required.")
	}
	if !category.Valid() {
		return invalid("gender_category", "gender_category must be male, female or mixed.")
	}
	if !race.IsTeam() {
		return invalid("race_id", "teams only exist in team races.")
	}
	return nil
}
