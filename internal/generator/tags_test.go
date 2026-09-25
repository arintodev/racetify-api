package generator

import "testing"

type tagFixtures struct {
	BibTags  []string `json:"bibTags"`
	TagCases []struct {
		Ctx    TagContext        `json:"ctx"`
		Values map[string]string `json:"values"`
	} `json:"tagCases"`
	FillCases []struct {
		Text string `json:"text"`
		Out  string `json:"out"`
	} `json:"fillCases"`
	FillCtx TagContext `json:"fillCtx"`
}

// The expected values come from the dashboard's own lib/bib-tags.ts.
func TestTagsMatchTypeScript(t *testing.T) {
	var fx tagFixtures
	loadFixtures(t, "tag_fixtures.json", &fx)

	// Same tag list, same order: a tag added to the dashboard must be added here.
	if len(fx.BibTags) != len(BIBTags) {
		t.Fatalf("dashboard has %d BIB tags, Go has %d: %v vs %v", len(fx.BibTags), len(BIBTags), fx.BibTags, BIBTags)
	}
	for i, key := range fx.BibTags {
		if BIBTags[i] != key {
			t.Fatalf("tag %d: Go has %s, dashboard has %s", i, BIBTags[i], key)
		}
	}

	for i, tc := range fx.TagCases {
		for _, key := range fx.BibTags {
			if got, want := TagValue(key, tc.Ctx), tc.Values[key]; got != want {
				t.Errorf("case %d %s = %q, want %q", i, key, got, want)
			}
		}
	}
}

func TestFillTagsMatchesTypeScript(t *testing.T) {
	var fx tagFixtures
	loadFixtures(t, "tag_fixtures.json", &fx)
	for _, tc := range fx.FillCases {
		if got := FillTags(tc.Text, fx.FillCtx); got != tc.Out {
			t.Errorf("FillTags(%q) = %q, want %q", tc.Text, got, tc.Out)
		}
	}
}

func TestTagKinds(t *testing.T) {
	if !IsQRTag("QR_CODE_BIB") || IsQRTag("BIB_NUMBER") {
		t.Error("only QR_CODE_* tags are QR codes")
	}
	if !IsBIBTag("TEAM_NAME") || IsBIBTag("NET_TIME") || IsBIBTag("nope") {
		t.Error("NET_TIME belongs to certificates, not BIBs")
	}
}
