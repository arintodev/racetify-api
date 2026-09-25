package generator

import (
	"regexp"
	"strconv"
	"strings"
)

// The BIB template tags and what each one prints. A port of the BIB half of
// the dashboard's lib/bib-tags.ts; tags_test.go compares every tag against
// fixtures generated from that TypeScript. An empty value prints as "".

// Participant is the part of a participant the BIB tags read. JSON names
// follow the dashboard's Participant type so fixtures decode directly.
type Participant struct {
	BibNumber             int     `json:"bibNumber"`
	BibName               *string `json:"bibName"`
	FirstName             string  `json:"firstName"`
	LastName              *string `json:"lastName"`
	Gender                string  `json:"gender"`
	BloodType             *string `json:"bloodType"`
	Club                  *string `json:"club"`
	LegOrder              *int    `json:"legOrder"`
	EmergencyContactName  *string `json:"emergencyContactName"`
	EmergencyContactPhone *string `json:"emergencyContactPhone"`
}

// TeamInfo is the team a participant belongs to, if any.
type TeamInfo struct {
	Name           string `json:"name"`
	GenderCategory string `json:"genderCategory"`
}

// RaceInfo is the participant's race, if known.
type RaceInfo struct {
	Name string `json:"name"`
}

// TagContext is everything the tags of one BIB are filled from.
type TagContext struct {
	Participant Participant `json:"participant"`
	Race        *RaceInfo   `json:"race"`
	Team        *TeamInfo   `json:"team"`
	EventName   string      `json:"eventName"`
}

var genderLabel = map[string]string{"male": "Laki-laki", "female": "Perempuan"}

var genderCategoryLabel = map[string]string{"male": "Putra", "female": "Putri", "mixed": "Campuran"}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// fullName joins first and last name, leaving out a missing part.
func fullName(p Participant) string {
	parts := []string{}
	if p.FirstName != "" {
		parts = append(parts, p.FirstName)
	}
	if last := str(p.LastName); last != "" {
		parts = append(parts, last)
	}
	return strings.Join(parts, " ")
}

// BIBTags is every tag a BIB template may use, in the editor's order.
var BIBTags = []string{
	"BIB_NUMBER", "BIB_NAME", "FULL_NAME", "FIRST_NAME", "LAST_NAME", "GENDER", "BLOOD_TYPE", "CLUB",
	"LEG_ORDER", "EMERGENCY_CONTACT_NAME", "EMERGENCY_CONTACT_PHONE", "TEAM_NAME", "TEAM_GENDER",
	"RACE_NAME", "EVENT_NAME", "QR_CODE_BIB",
}

var bibTagSet = func() map[string]bool {
	set := make(map[string]bool, len(BIBTags))
	for _, key := range BIBTags {
		set[key] = true
	}
	return set
}()

// IsBIBTag reports whether key is a tag of the BIB service.
func IsBIBTag(key string) bool { return bibTagSet[key] }

// IsQRTag reports whether a tag is drawn as a QR code rather than text.
func IsQRTag(key string) bool { return strings.HasPrefix(key, "QR_CODE_") }

// TagValue is what a tag prints for one participant. Unknown tags print "".
func TagValue(key string, c TagContext) string {
	p := c.Participant
	switch key {
	case "BIB_NUMBER", "QR_CODE_BIB":
		return strconv.Itoa(p.BibNumber)
	case "BIB_NAME":
		return str(p.BibName)
	case "FULL_NAME":
		return fullName(p)
	case "FIRST_NAME":
		return p.FirstName
	case "LAST_NAME":
		return str(p.LastName)
	case "GENDER":
		return genderLabel[p.Gender]
	case "BLOOD_TYPE":
		return str(p.BloodType)
	case "CLUB":
		return str(p.Club)
	case "LEG_ORDER":
		if p.LegOrder == nil {
			return ""
		}
		return strconv.Itoa(*p.LegOrder)
	case "EMERGENCY_CONTACT_NAME":
		return str(p.EmergencyContactName)
	case "EMERGENCY_CONTACT_PHONE":
		return str(p.EmergencyContactPhone)
	case "TEAM_NAME":
		if c.Team == nil {
			return ""
		}
		return c.Team.Name
	case "TEAM_GENDER":
		if c.Team == nil {
			return ""
		}
		return genderCategoryLabel[c.Team.GenderCategory]
	case "RACE_NAME":
		if c.Race == nil {
			return ""
		}
		return c.Race.Name
	case "EVENT_NAME":
		return c.EventName
	}
	return ""
}

// TagPattern matches a {{TAG}} placeholder.
var TagPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// FillTags replaces every {{TAG}} of a BIB in text with its value; a tag the
// BIB service does not have is left as written.
func FillTags(text string, c TagContext) string {
	return TagPattern.ReplaceAllStringFunc(text, func(match string) string {
		key := strings.ToUpper(TagPattern.FindStringSubmatch(match)[1])
		if !IsBIBTag(key) {
			return match
		}
		return TagValue(key, c)
	})
}
