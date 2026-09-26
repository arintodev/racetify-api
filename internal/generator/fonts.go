package generator

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/go-fonts/dejavu/dejavusans"
	"github.com/go-fonts/dejavu/dejavusansbold"
	"github.com/go-fonts/dejavu/dejavusansboldoblique"
	"github.com/go-fonts/dejavu/dejavusansoblique"
	"github.com/go-fonts/liberation/liberationmonobold"
	"github.com/go-fonts/liberation/liberationmonobolditalic"
	"github.com/go-fonts/liberation/liberationmonoitalic"
	"github.com/go-fonts/liberation/liberationmonoregular"
	"github.com/go-fonts/liberation/liberationsansbold"
	"github.com/go-fonts/liberation/liberationsansbolditalic"
	"github.com/go-fonts/liberation/liberationsansitalic"
	"github.com/go-fonts/liberation/liberationsansregular"
	"github.com/go-fonts/liberation/liberationserifbold"
	"github.com/go-fonts/liberation/liberationserifbolditalic"
	"github.com/go-fonts/liberation/liberationserifitalic"
	"github.com/go-fonts/liberation/liberationserifregular"
)

// Fonts are referred to by a stable key, never by CSS name: "bundled:<name>"
// for the fonts built into the binary, "lib:<id>" for the platform's font
// library (internal/fontlib), so a template stores the same thing whichever
// it uses.

// FontStyle is which face of a family a text is set in. A family in the library
// may have any subset of the four; the renderer never swaps one for another
// (a bold text is not drawn with the regular file, or faked with a stroke).
type FontStyle struct{ Bold, Italic bool }

// The style names the library and templates use.
const (
	StyleRegular    = "regular"
	StyleBold       = "bold"
	StyleItalic     = "italic"
	StyleBoldItalic = "bold_italic"
)

// StyleNames lists every style, in the order the dashboard shows them.
var StyleNames = []string{StyleRegular, StyleBold, StyleItalic, StyleBoldItalic}

// Name is the style's name: regular, bold, italic or bold_italic.
func (s FontStyle) Name() string {
	switch {
	case s.Bold && s.Italic:
		return StyleBoldItalic
	case s.Bold:
		return StyleBold
	case s.Italic:
		return StyleItalic
	}
	return StyleRegular
}

// ParseFontStyle reads a style name.
func ParseFontStyle(name string) (FontStyle, bool) {
	switch name {
	case StyleRegular:
		return FontStyle{}, true
	case StyleBold:
		return FontStyle{Bold: true}, true
	case StyleItalic:
		return FontStyle{Italic: true}, true
	case StyleBoldItalic:
		return FontStyle{Bold: true, Italic: true}, true
	}
	return FontStyle{}, false
}

var (
	// ErrFontUnknown: a text names a font that has no key (an unknown CSS family).
	ErrFontUnknown = errors.New("generator: font is not in the library")
	// ErrFontStyleMissing: the font exists but has no file for the style asked.
	ErrFontStyleMissing = errors.New("generator: font has no file for this style")
)

// FontNeed is a font style a template needs: Key is empty when the template
// names a font that has no key (one the library does not have).
type FontNeed struct {
	Key    string
	Family string
	Style  string
}

// FontChecker says what of a template's font needs cannot be met, in words
// its author can act on (nil: all is there). fontlib.Service is one.
type FontChecker interface {
	Check(ctx context.Context, needs []FontNeed) []string
}

// FontProvider is how the renderer gets the bytes of a font.
type FontProvider interface {
	// TTF returns the TrueType data of a font key in a style, or
	// ErrFontUnknown / ErrFontStyleMissing.
	TTF(key string, style FontStyle) ([]byte, error)
}

// Bundled font keys (SIL OFL fonts shipped inside the binary).
const (
	FontLiberationSans  = "bundled:liberation-sans"
	FontLiberationSerif = "bundled:liberation-serif"
	FontLiberationMono  = "bundled:liberation-mono"
	FontDejaVuSans      = "bundled:dejavu-sans"
)

// BundledFamily is a font built into the binary.
type BundledFamily struct {
	Key    string
	Family string
	files  map[string][]byte // by style name
}

// Styles lists the styles the family has.
func (f BundledFamily) Styles() []string {
	var out []string
	for _, name := range StyleNames {
		if _, ok := f.files[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// File is the font file of a style.
func (f BundledFamily) File(style string) ([]byte, bool) {
	data, ok := f.files[style]
	return data, ok
}

var bundledFamilies = []BundledFamily{
	{FontLiberationSans, "Liberation Sans", map[string][]byte{
		StyleRegular: liberationsansregular.TTF, StyleBold: liberationsansbold.TTF,
		StyleItalic: liberationsansitalic.TTF, StyleBoldItalic: liberationsansbolditalic.TTF}},
	{FontLiberationSerif, "Liberation Serif", map[string][]byte{
		StyleRegular: liberationserifregular.TTF, StyleBold: liberationserifbold.TTF,
		StyleItalic: liberationserifitalic.TTF, StyleBoldItalic: liberationserifbolditalic.TTF}},
	{FontLiberationMono, "Liberation Mono", map[string][]byte{
		StyleRegular: liberationmonoregular.TTF, StyleBold: liberationmonobold.TTF,
		StyleItalic: liberationmonoitalic.TTF, StyleBoldItalic: liberationmonobolditalic.TTF}},
	{FontDejaVuSans, "DejaVu Sans", map[string][]byte{
		StyleRegular: dejavusans.TTF, StyleBold: dejavusansbold.TTF,
		StyleItalic: dejavusansoblique.TTF, StyleBoldItalic: dejavusansboldoblique.TTF}},
}

// BundledFamilies lists the fonts built into the binary.
func BundledFamilies() []BundledFamily { return bundledFamilies }

// BundledFamilyByKey finds one bundled family.
func BundledFamilyByKey(key string) (BundledFamily, bool) {
	for _, f := range bundledFamilies {
		if f.Key == key {
			return f, true
		}
	}
	return BundledFamily{}, false
}

// BundledFonts serves the fonts built into the binary.
type BundledFonts struct{}

func (BundledFonts) TTF(key string, style FontStyle) ([]byte, error) {
	family, ok := BundledFamilyByKey(key)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrFontUnknown, key)
	}
	data, ok := family.File(style.Name())
	if !ok {
		return nil, fmt.Errorf("%w: %s (%s)", ErrFontStyleMissing, family.Family, style.Name())
	}
	return data, nil
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
	"sans-serif":      FontLiberationSans,
	"verdana":         FontDejaVuSans,
	"georgia":         FontLiberationSerif,
	"times new roman": FontLiberationSerif,
	"serif":           FontLiberationSerif,
	"courier new":     FontLiberationMono,
	"courier":         FontLiberationMono,
	"monospace":       FontLiberationMono,
}

// ResolveFontKey picks the font key for a template text: an explicit key
// (data-font) wins, otherwise the first family of the CSS font-family list
// when it is one the editor offered before font keys. Any other family has no
// key: it is not in the library, and the text cannot be drawn until the
// template names one that is (there is no silent fallback).
func ResolveFontKey(explicitKey, cssFamily string) (string, bool) {
	if explicitKey != "" {
		return explicitKey, true
	}
	first := strings.ToLower(strings.TrimSpace(strings.Split(cssFamily, ",")[0]))
	first = strings.Trim(first, `"'`)
	key, ok := legacyFonts[first]
	return key, ok
}

// FamilyName is the first family of a CSS font-family list, as written.
func FamilyName(cssFamily string) string {
	first := strings.TrimSpace(strings.Split(cssFamily, ",")[0])
	return strings.Trim(first, `"'`)
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
