// Package participant is the bounded context for an event's people and
// teams: participants (one row per person), teams (the unit of competition
// in relay and team-loop races), and the bulk import that registers many of
// them at once (docs/phase1-api-plan.md §4.2, docs/team-loop-participants-
// plan.md). Rules that need no database - format validation, team
// composition, import row matching, list filters - are pure functions so
// they can be tested without one.
package participant

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Gender string

const (
	GenderMale   Gender = "male"
	GenderFemale Gender = "female"
)

func (g Gender) Valid() bool { return g == GenderMale || g == GenderFemale }

type Status string

const (
	StatusRegistered Status = "registered"
	StatusFinisher   Status = "finisher"
	StatusDNF        Status = "dnf"
	StatusDNS        Status = "dns"
)

func (s Status) Valid() bool {
	switch s {
	case StatusRegistered, StatusFinisher, StatusDNF, StatusDNS:
		return true
	}
	return false
}

// GenderCategory is a team's category: all-male, all-female, or mixed.
type GenderCategory string

const (
	CategoryMale   GenderCategory = "male"
	CategoryFemale GenderCategory = "female"
	CategoryMixed  GenderCategory = "mixed"
)

func (c GenderCategory) Valid() bool {
	return c == CategoryMale || c == CategoryFemale || c == CategoryMixed
}

// Participant is one person registered in an event. Result columns
// (times, laps, ranks) are written by the results import, never by the
// registration CRUD in this package.
type Participant struct {
	ID       string
	TenantID string
	EventID  string
	RaceID   string

	TeamID   *string
	LegOrder *int
	// TeamName is the name of TeamID's team, filled by reads for display;
	// it is never written from here.
	TeamName *string

	FirstName string
	LastName  *string
	BibName   *string
	BibNumber int
	Gender    Gender
	RefID     *string

	Email                 *string
	Club                  *string
	EmergencyContactName  *string
	EmergencyContactPhone *string
	BloodType             *string

	Status Status

	GunTimeMS    *int64
	NetTimeMS    *int64
	TotalLaps    *int
	OverallRank  *int
	CategoryRank *int

	CreatedAt time.Time
	UpdatedAt time.Time
}

type Team struct {
	ID       string
	TenantID string
	EventID  string
	RaceID   string

	Name           string
	GenderCategory GenderCategory
	Status         Status

	GunTimeMS    *int64
	NetTimeMS    *int64
	TotalLaps    *int
	OverallRank  *int
	CategoryRank *int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RaceFormat is the part of a race this package needs to validate
// participants: who competes (person or team), how the course is run, and
// the fixed team size. It is read straight from the races table.
type RaceFormat struct {
	ID         string
	EventID    string
	Name       string
	EntryType  string // individual | team
	CourseType string // standard | relay | loop
	TeamSize   *int
}

func (r RaceFormat) IsTeam() bool  { return r.EntryType == "team" }
func (r RaceFormat) IsRelay() bool { return r.CourseType == "relay" }

// Error is a request-level failure with a stable machine code and the field
// it concerns, so a form can point at the input that is wrong.
type Error struct {
	Status  int
	Code    string
	Field   string
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(field, message string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Field: field, Message: message}
}

func conflict(code, field, message string) *Error {
	return &Error{Status: http.StatusConflict, Code: code, Field: field, Message: message}
}

// bloodTypes are the accepted stored spellings: ABO group with an optional
// rhesus sign (a group alone is kept when that is all a registration form
// collected).
var bloodTypes = map[string]bool{
	"A+": true, "A-": true, "B+": true, "B-": true, "AB+": true, "AB-": true, "O+": true, "O-": true,
	"A": true, "B": true, "AB": true, "O": true,
}

// NormalizeBloodType accepts the spellings registration forms produce
// ("A+", "a pos", "AB Rh-", "O positif", "b negatif", "a") and returns one
// of the stored values, or false when it is not a blood type.
func NormalizeBloodType(raw string) (string, bool) {
	s := strings.ToUpper(strings.Join(strings.Fields(raw), ""))
	group := ""
	for _, g := range []string{"AB", "A", "B", "O"} { // AB first: "A" is its prefix
		if strings.HasPrefix(s, g) {
			group = g
			s = strings.TrimPrefix(s, g)
			break
		}
	}
	if group == "" {
		return "", false
	}
	s = strings.TrimPrefix(s, "RH")
	sign := ""
	switch s {
	case "":
	case "+", "POS", "POSITIF", "POSITIVE":
		sign = "+"
	case "-", "NEG", "NEGATIF", "NEGATIVE":
		sign = "-"
	default:
		return "", false
	}
	out := group + sign
	return out, bloodTypes[out]
}

func fmtName(first string, last *string) string {
	if last == nil || *last == "" {
		return first
	}
	return fmt.Sprintf("%s %s", first, *last)
}

func ptr[T any](v T) *T { return &v }

func trimSpace(s string) string { return strings.TrimSpace(s) }
