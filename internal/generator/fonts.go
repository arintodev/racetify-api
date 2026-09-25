package generator

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/go-fonts/dejavu/dejavusans"
	"github.com/go-fonts/dejavu/dejavusansbold"
	"github.com/go-fonts/liberation/liberationmonobold"
	"github.com/go-fonts/liberation/liberationmonoregular"
	"github.com/go-fonts/liberation/liberationsansbold"
	"github.com/go-fonts/liberation/liberationsansregular"
	"github.com/go-fonts/liberation/liberationserifbold"
	"github.com/go-fonts/liberation/liberationserifregular"
)

// Fonts are referred to by a stable key, never by CSS name, so fonts a
// workspace uploads later ("tenant:<id>") can sit beside the bundled ones
// ("bundled:<name>") without changing what a template stores.
// FontProvider is how the renderer gets the bytes of a font.
type FontProvider interface {
	// TTF returns the TrueType data of a font key in regular or bold.
	TTF(key string, bold bool) ([]byte, error)
}

// Bundled font keys (SIL OFL fonts shipped inside the binary).
const (
	FontLiberationSans  = "bundled:liberation-sans"
	FontLiberationSerif = "bundled:liberation-serif"
	FontLiberationMono  = "bundled:liberation-mono"
	FontDejaVuSans      = "bundled:dejavu-sans"

	// DefaultFont is used when a template names nothing we know.
	DefaultFont = FontLiberationSans
)

type fontPair struct{ regular, bold []byte }

// BundledFonts serves the fonts built into the binary.
type BundledFonts struct{}

var bundled = map[string]fontPair{
	FontLiberationSans:  {liberationsansregular.TTF, liberationsansbold.TTF},
	FontLiberationSerif: {liberationserifregular.TTF, liberationserifbold.TTF},
	FontLiberationMono:  {liberationmonoregular.TTF, liberationmonobold.TTF},
	FontDejaVuSans:      {dejavusans.TTF, dejavusansbold.TTF},
}

func (BundledFonts) TTF(key string, bold bool) ([]byte, error) {
	pair, ok := bundled[key]
	if !ok {
		return nil, fmt.Errorf("generator: unknown font %q", key)
	}
	if bold {
		return pair.bold, nil
	}
	return pair.regular, nil
}

// legacyFonts maps the CSS families the editor offered before font keys
// existed (lib/bib-template.ts fontOptions) to the closest bundled font.
// Arial, Courier New and Times metrics are matched by the Liberation fonts;
// the others are the nearest free look (see docs/bib-pdf-gopdf-plan.md §3).
var legacyFonts = map[string]string{
	"arial":           FontLiberationSans,
	"arial black":     FontLiberationSans,
	"impact":          FontLiberationSans,
	"helvetica":       FontLiberationSans,
	"verdana":         FontDejaVuSans,
	"georgia":         FontLiberationSerif,
	"times new roman": FontLiberationSerif,
	"courier new":     FontLiberationMono,
	"courier":         FontLiberationMono,
}

// FontKeyFor picks the font key for a template text: an explicit key wins,
// otherwise the first family of the CSS font-family list.
func FontKeyFor(explicitKey, cssFamily string) string {
	if explicitKey != "" {
		return explicitKey
	}
	first := strings.ToLower(strings.TrimSpace(strings.Split(cssFamily, ",")[0]))
	first = strings.Trim(first, `"'`)
	if key, ok := legacyFonts[first]; ok {
		return key
	}
	return DefaultFont
}

// FontMetrics are a font's vertical metrics as fractions of the font size
// (the hhea ascender and descender, which is what browsers place text by).
type FontMetrics struct {
	Ascent  float64
	Descent float64 // positive
}

// ReadMetrics reads them from TrueType data.
func ReadMetrics(ttf []byte) (FontMetrics, error) {
	if len(ttf) < 12 {
		return FontMetrics{}, fmt.Errorf("generator: font data too short")
	}
	tables := int(binary.BigEndian.Uint16(ttf[4:6]))
	var head, hhea []byte
	for i := 0; i < tables; i++ {
		entry := 12 + 16*i
		if entry+16 > len(ttf) {
			break
		}
		offset := int(binary.BigEndian.Uint32(ttf[entry+8 : entry+12]))
		length := int(binary.BigEndian.Uint32(ttf[entry+12 : entry+16]))
		if offset < 0 || offset+length > len(ttf) {
			continue
		}
		switch string(ttf[entry : entry+4]) {
		case "head":
			head = ttf[offset : offset+length]
		case "hhea":
			hhea = ttf[offset : offset+length]
		}
	}
	if len(head) < 20 || len(hhea) < 8 {
		return FontMetrics{}, fmt.Errorf("generator: font has no head/hhea table")
	}
	unitsPerEm := float64(binary.BigEndian.Uint16(head[18:20]))
	if unitsPerEm == 0 {
		return FontMetrics{}, fmt.Errorf("generator: font has zero unitsPerEm")
	}
	ascender := float64(int16(binary.BigEndian.Uint16(hhea[4:6])))
	descender := float64(int16(binary.BigEndian.Uint16(hhea[6:8])))
	return FontMetrics{Ascent: ascender / unitsPerEm, Descent: -descender / unitsPerEm}, nil
}
