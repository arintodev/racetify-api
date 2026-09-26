package generator

import (
	"math"
	"strings"
	"testing"
)

type svgFixtures struct {
	SVG      string  `json:"svg"`
	WidthMM  float64 `json:"widthMm"`
	HeightMM float64 `json:"heightMm"`
	Expected []struct {
		Kind      string    `json:"kind"`
		Tag       *string   `json:"tag"`
		Box       []float64 `json:"box"`
		FontSize  *float64  `json:"fontSizePt"`
		Align     *string   `json:"align"`
		VAlign    *string   `json:"valign"`
		Bold      *bool     `json:"bold"`
		Italic    bool      `json:"italic"`
		FontKey   *string   `json:"fontKey"`
		AutoScale *bool     `json:"autoScale"`
		Color     *string   `json:"color"`
		Lines     []string  `json:"lines"`
		Family    *string   `json:"family"`
	} `json:"expected"`
}

// The SVG comes from the dashboard's own templateToSvg.
func TestParseTemplateReadsWhatTheDashboardWrites(t *testing.T) {
	var fx svgFixtures
	loadFixtures(t, "svg_fixtures.json", &fx)

	art, err := ParseTemplate([]byte(fx.SVG))
	if err != nil {
		t.Fatal(err)
	}
	if art.WidthMM != fx.WidthMM || art.HeightMM != fx.HeightMM {
		t.Fatalf("size %vx%v, want %vx%v", art.WidthMM, art.HeightMM, fx.WidthMM, fx.HeightMM)
	}
	if len(art.Items) != len(fx.Expected) {
		t.Fatalf("%d items, want %d", len(art.Items), len(fx.Expected))
	}
	if art.StaticRuns() != 0 || art.HasEmbeddedTags() {
		t.Error("a template without design or images has no static layers")
	}

	for i, want := range fx.Expected {
		box := Box{X: want.Box[0], Y: want.Box[1], W: want.Box[2], H: want.Box[3]}
		if want.Kind == "qr" {
			q, ok := art.Items[i].(QRItem)
			if !ok || q.Tag != *want.Tag || q.Box != box {
				t.Errorf("item %d: %+v, want QR %s %+v", i, art.Items[i], *want.Tag, box)
			}
			continue
		}
		it, ok := art.Items[i].(TextItem)
		if !ok {
			t.Fatalf("item %d is %T, want a text", i, art.Items[i])
		}
		if it.Box != box {
			t.Errorf("item %d box %+v, want %+v", i, it.Box, box)
		}
		if math.Abs(it.FontSizePt-*want.FontSize) > 0.02 {
			t.Errorf("item %d size %v pt, want %v", i, it.FontSizePt, *want.FontSize)
		}
		if it.Align != *want.Align || it.VAlign != *want.VAlign || it.Bold != *want.Bold || it.AutoScale != *want.AutoScale {
			t.Errorf("item %d style %+v, want align=%s valign=%s bold=%v auto=%v", i, it, *want.Align, *want.VAlign, *want.Bold, *want.AutoScale)
		}
		if it.Italic != want.Italic {
			t.Errorf("item %d italic %v, want %v", i, it.Italic, want.Italic)
		}
		if wantKey := ""; want.FontKey != nil {
			wantKey = *want.FontKey
			if it.FontKey != wantKey {
				t.Errorf("item %d font key %q, want %q", i, it.FontKey, wantKey)
			}
		} else if it.FontKey != "" {
			t.Errorf("item %d has font key %q, want none", i, it.FontKey)
		}
		if strings.Join(it.Lines, "|") != strings.Join(want.Lines, "|") {
			t.Errorf("item %d lines %q, want %q", i, it.Lines, want.Lines)
		}
		if it.FontFamily != *want.Family && *want.Family != "" {
			t.Errorf("item %d family %q, want %q", i, it.FontFamily, *want.Family)
		}
	}

	// Colours: #cc0000, and the three-digit #0a0.
	if got := art.Items[1].(TextItem).Color; got != (RGB{0xcc, 0, 0}) {
		t.Errorf("colour %+v", got)
	}
	if got := art.Items[2].(TextItem).Color; got != (RGB{0, 0xaa, 0}) {
		t.Errorf("short colour %+v", got)
	}
}

func TestParseTemplateStaticRuns(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="100mm" height="70mm" viewBox="0 0 100 70">` +
		`<svg x="0" y="0" width="100" height="70"><rect width="100" height="70"/></svg>` +
		`<image data-element="image" data-box="1 1 10 10" href="data:image/png;base64,AAAA" x="1" y="1" width="10" height="10"/>` +
		`<text data-box="0 0 50 10" x="0" y="0" font-size="5" fill="#000">{{BIB_NUMBER}}</text>` +
		`<image data-element="image" data-box="1 1 10 10" href="data:image/png;base64,AAAA" x="1" y="1" width="10" height="10"/>` +
		`</svg>`
	art, err := ParseTemplate([]byte(svg))
	if err != nil {
		t.Fatal(err)
	}
	// design + image are one run; the image above the text is another.
	if art.StaticRuns() != 2 || len(art.Items) != 4 {
		t.Fatalf("runs=%d items=%d, want 2 and 4", art.StaticRuns(), len(art.Items))
	}
	if art.HasEmbeddedTags() {
		t.Error("no tags inside the design")
	}
}

func TestParseTemplateDetectsTagsInDesign(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="100mm" height="70mm" viewBox="0 0 100 70">` +
		`<svg><g transform="rotate(90)"><text>{{FULL_NAME}}</text></g></svg></svg>`
	art, err := ParseTemplate([]byte(svg))
	if err != nil {
		t.Fatal(err)
	}
	if !art.HasEmbeddedTags() {
		t.Error("a tag inside the design layer must be detected")
	}
}

func TestParseTemplateRejectsBadInput(t *testing.T) {
	for name, svg := range map[string]string{
		"not svg":      `<html></html>`,
		"empty":        ``,
		"no size":      `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
		"text no box":  `<svg width="10mm" height="10mm"><text font-size="3">x</text></svg>`,
		"text no size": `<svg width="10mm" height="10mm"><text data-box="0 0 5 5">x</text></svg>`,
		"qr no box":    `<svg width="10mm" height="10mm"><rect data-tag="QR_CODE_BIB"/></svg>`,
		"broken xml":   `<svg width="10mm" height="10mm"><text data-box="0 0 5 5" font-size="3">x`,
	} {
		if _, err := ParseTemplate([]byte(svg)); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}
