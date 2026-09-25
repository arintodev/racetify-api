package participant

import (
	"strings"
	"testing"
)

func TestNormalizeBloodType(t *testing.T) {
	ok := map[string]string{
		"A+": "A+", "a pos": "A+", "AB Rh-": "AB-", "O positif": "O+", "b negatif": "B-",
		"a": "A", "ab": "AB", "o+": "O+", " O  Rh + ": "O+",
	}
	for in, want := range ok {
		if got, valid := NormalizeBloodType(in); !valid || got != want {
			t.Errorf("NormalizeBloodType(%q) = (%q, %v), want (%q, true)", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "C+", "A++", "positif", "AB ?", "x"} {
		if got, valid := NormalizeBloodType(in); valid {
			t.Errorf("NormalizeBloodType(%q) = %q, want invalid", in, got)
		}
	}
}

func TestInputNormalize(t *testing.T) {
	in := Input{
		FirstName: "  Budi ", LastName: ptr("  "), Email: ptr(" b@x.co "), Club: ptr(""),
		BloodType: ptr("a pos"),
	}
	if bad := in.Normalize(); bad != "" {
		t.Fatalf("unexpected bad blood type %q", bad)
	}
	if in.FirstName != "Budi" || in.LastName != nil || *in.Email != "b@x.co" || in.Club != nil || *in.BloodType != "A+" {
		t.Errorf("normalize wrong: %+v", in)
	}
	if in.Status != StatusRegistered {
		t.Errorf("default status = %q", in.Status)
	}
	in2 := Input{BloodType: ptr("zzz")}
	if bad := in2.Normalize(); bad != "zzz" || in2.BloodType != nil {
		t.Errorf("unrecognised blood type should be dropped and reported, got %q / %v", bad, in2.BloodType)
	}
}

func TestInputValidate(t *testing.T) {
	individual := RaceFormat{EntryType: "individual", CourseType: "standard"}
	team := RaceFormat{EntryType: "team", CourseType: "loop", TeamSize: ptr(4)}
	relay := RaceFormat{EntryType: "team", CourseType: "relay", TeamSize: ptr(4)}
	base := func() Input {
		return Input{FirstName: "Budi", BibNumber: 7, Gender: GenderMale, Status: StatusRegistered}
	}
	with := func(f func(*Input)) Input { in := base(); f(&in); return in }

	tests := []struct {
		name  string
		in    Input
		race  RaceFormat
		field string // "" means valid
	}{
		{"valid individual", base(), individual, ""},
		{"missing first name", with(func(i *Input) { i.FirstName = "" }), individual, "first_name"},
		{"bib must be positive", with(func(i *Input) { i.BibNumber = 0 }), individual, "bib_number"},
		{"bad gender", with(func(i *Input) { i.Gender = "x" }), individual, "gender"},
		{"bad status", with(func(i *Input) { i.Status = "gone" }), individual, "status"},
		{"bad email", with(func(i *Input) { i.Email = ptr("nope") }), individual, "email"},
		{"individual rejects team", with(func(i *Input) { i.TeamID = ptr("t") }), individual, "team_id"},
		{"team race requires team", base(), team, "team_id"},
		{"team race with team ok", with(func(i *Input) { i.TeamID = ptr("t") }), team, ""},
		{"loop team rejects leg", with(func(i *Input) { i.TeamID = ptr("t"); i.LegOrder = ptr(1) }), team, "leg_order"},
		{"relay requires leg", with(func(i *Input) { i.TeamID = ptr("t") }), relay, "leg_order"},
		{"relay leg in range", with(func(i *Input) { i.TeamID = ptr("t"); i.LegOrder = ptr(4) }), relay, ""},
		{"relay leg too high", with(func(i *Input) { i.TeamID = ptr("t"); i.LegOrder = ptr(5) }), relay, "leg_order"},
		{"relay leg zero", with(func(i *Input) { i.TeamID = ptr("t"); i.LegOrder = ptr(0) }), relay, "leg_order"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			err := in.Validate(tt.race)
			switch {
			case tt.field == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.field != "" && (err == nil || err.Field != tt.field):
				t.Errorf("got %v, want an error on field %q", err, tt.field)
			}
		})
	}
}

func TestCheckTeamComposition(t *testing.T) {
	m, f := GenderMale, GenderFemale
	tests := []struct {
		name     string
		category GenderCategory
		size     int
		genders  []Gender
		wantErr  bool
		wantWarn bool
	}{
		{"male team all male", CategoryMale, 4, []Gender{m, m}, false, false},
		{"male team with a woman", CategoryMale, 4, []Gender{m, f}, true, false},
		{"female team with a man", CategoryFemale, 4, []Gender{f, m}, true, false},
		{"mixed complete with both", CategoryMixed, 2, []Gender{m, f}, false, false},
		{"mixed complete one gender", CategoryMixed, 2, []Gender{m, m}, true, false},
		{"mixed incomplete one gender only warns", CategoryMixed, 4, []Gender{m, m}, false, true},
		{"mixed empty warns", CategoryMixed, 4, nil, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err, warn := CheckTeamComposition(tt.category, tt.size, tt.genders)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if (warn != "") != tt.wantWarn {
				t.Errorf("warn = %q, wantWarn %v", warn, tt.wantWarn)
			}
		})
	}
}

func TestValidateTeam(t *testing.T) {
	team := RaceFormat{EntryType: "team", CourseType: "relay"}
	if err := ValidateTeam("Lawu", CategoryMixed, team); err != nil {
		t.Errorf("valid team rejected: %v", err)
	}
	for name, err := range map[string]*Error{
		"blank name":   ValidateTeam("  ", CategoryMale, team),
		"bad category": ValidateTeam("A", "coed", team),
		"individual":   ValidateTeam("A", CategoryMale, RaceFormat{EntryType: "individual"}),
	} {
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	_ = strings.TrimSpace
}
