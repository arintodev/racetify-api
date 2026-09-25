package generator

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Reader for the "SVG berisi tag" the dashboard writes (lib/bib-template.ts,
// templateToSvg). It is deliberately not a general SVG parser: it understands
// exactly the structure that function emits —
//
//	<svg width="200mm" height="150mm" viewBox="0 0 200 150">
//	  <svg …>design layer</svg>            static
//	  <image data-element="image" …/>       static
//	  <rect data-tag="QR_CODE_BIB" data-box="x y w h"/>
//	  <text data-box="x y w h" …>{{TAG}} or lines</text>
//
// in stacking order (later draws on top). Everything static (design layer and
// images) is drawn by the browser into raster layers; only text and QR are
// drawn here, as vectors.

// mmPerPt converts points to millimetres (the dashboard's MM_PER_PT).
const mmPerPt = 25.4 / 72

// TextLineHeight is the line pitch of multi-line text, in font sizes
// (the dashboard's TEXT_LINE_HEIGHT).
const TextLineHeight = 1.2

// MinFontPt is the smallest size auto-scale shrinks text to.
const MinFontPt = 4.0

// Box is a rectangle in millimetres from the template's top-left corner.
type Box struct{ X, Y, W, H float64 }

// RGB is an 8-bit colour.
type RGB struct{ R, G, B uint8 }

// Item is one thing in a template's stacking order.
type Item interface{ isItem() }

// StaticItem is the design layer or an image: drawn from a raster layer.
type StaticItem struct {
	Kind string // "design" | "image"
	// HasTags: the layer's own markup holds {{TAG}} text, which a raster
	// cannot fill per participant.
	HasTags bool
}

// QRItem is a QR code placeholder to fill with a tag's value.
type QRItem struct {
	Tag string
	Box Box
}

// TextItem is a tag or static text (which may hold {{TAG}}s).
type TextItem struct {
	Box        Box
	Lines      []string
	FontKey    string // explicit key from data-font, if any
	FontFamily string // CSS font-family as written
	FontSizePt float64
	Bold       bool
	Color      RGB
	Align      string // left | center | right
	VAlign     string // top | middle | bottom
	AutoScale  bool
}

func (StaticItem) isItem() {}
func (QRItem) isItem()     {}
func (TextItem) isItem()   {}

// Artwork is a parsed template SVG: its size and what it draws, in stacking order.
type Artwork struct {
	WidthMM, HeightMM float64
	Items             []Item
}

// StaticRuns is how many raster layers the template needs: each maximal run of
// consecutive static items is drawn from one layer (text above stays above).
func (t *Artwork) StaticRuns() int {
	runs, in := 0, false
	for _, item := range t.Items {
		if _, static := item.(StaticItem); static {
			if !in {
				runs++
			}
			in = true
		} else {
			in = false
		}
	}
	return runs
}

// HasEmbeddedTags reports whether any static layer holds {{TAG}} text.
func (t *Artwork) HasEmbeddedTags() bool {
	for _, item := range t.Items {
		if s, ok := item.(StaticItem); ok && s.HasTags {
			return true
		}
	}
	return false
}

func attr(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func parseBox(value string) (Box, bool) {
	f := strings.Fields(value)
	if len(f) != 4 {
		return Box{}, false
	}
	var n [4]float64
	for i, s := range f {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return Box{}, false
		}
		n[i] = v
	}
	return Box{X: n[0], Y: n[1], W: n[2], H: n[3]}, true
}

func parseColor(value string) RGB {
	v := strings.TrimPrefix(strings.TrimSpace(value), "#")
	if len(v) == 3 {
		v = string([]byte{v[0], v[0], v[1], v[1], v[2], v[2]})
	}
	if len(v) != 6 {
		return RGB{}
	}
	n, err := strconv.ParseUint(v, 16, 32)
	if err != nil {
		return RGB{}
	}
	return RGB{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n)}
}

// normalizeLine collapses whitespace the way SVG text does by default.
func normalizeLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ParseTemplate reads a template SVG.
func ParseTemplate(svg []byte) (*Artwork, error) {
	dec := xml.NewDecoder(bytes.NewReader(svg))
	dec.Strict = false

	var root *xml.StartElement
	for root == nil {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("generator: template is not valid SVG: %w", err)
		}
		if el, ok := tok.(xml.StartElement); ok {
			if el.Name.Local != "svg" {
				return nil, errors.New("generator: template root is not <svg>")
			}
			root = &el
		}
	}
	t := &Artwork{}
	if box, ok := parseBox(attr(*root, "viewBox")); ok && box.W > 0 && box.H > 0 {
		t.WidthMM, t.HeightMM = box.W, box.H
	} else {
		t.WidthMM = parseMM(attr(*root, "width"))
		t.HeightMM = parseMM(attr(*root, "height"))
	}
	if t.WidthMM <= 0 || t.HeightMM <= 0 {
		return nil, errors.New("generator: template has no size (viewBox or width/height in mm)")
	}

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return t, nil
		}
		if err != nil {
			return nil, fmt.Errorf("generator: template is not valid SVG: %w", err)
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch el.Name.Local {
		case "svg":
			hasTags, err := skipScanningTags(dec)
			if err != nil {
				return nil, err
			}
			t.Items = append(t.Items, StaticItem{Kind: "design", HasTags: hasTags})
		case "image":
			kind := "design"
			if attr(el, "data-element") == "image" {
				kind = "image"
			}
			if err := dec.Skip(); err != nil {
				return nil, err
			}
			t.Items = append(t.Items, StaticItem{Kind: kind})
		case "rect":
			tag := attr(el, "data-tag")
			if err := dec.Skip(); err != nil {
				return nil, err
			}
			if tag == "" {
				continue // an unnamed rectangle carries no data
			}
			box, ok := parseBox(attr(el, "data-box"))
			if !ok {
				return nil, fmt.Errorf("generator: QR element %s has no data-box", tag)
			}
			t.Items = append(t.Items, QRItem{Tag: strings.ToUpper(tag), Box: box})
		case "text":
			item, err := readText(dec, el)
			if err != nil {
				return nil, err
			}
			t.Items = append(t.Items, item)
		default:
			if err := dec.Skip(); err != nil {
				return nil, err
			}
		}
	}
}

func parseMM(value string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), "mm"), 64)
	if err != nil {
		return 0
	}
	return v
}

// skipScanningTags consumes the rest of the element just opened and reports
// whether any text inside it holds a {{TAG}}.
func skipScanningTags(dec *xml.Decoder) (bool, error) {
	depth, found := 1, false
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return false, fmt.Errorf("generator: template is not valid SVG: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if TagPattern.Match(t) {
				found = true
			}
		}
	}
	return found, nil
}

func readText(dec *xml.Decoder, el xml.StartElement) (TextItem, error) {
	box, ok := parseBox(attr(el, "data-box"))
	if !ok {
		return TextItem{}, errors.New("generator: a text element has no data-box")
	}
	sizeMM, err := strconv.ParseFloat(attr(el, "font-size"), 64)
	if err != nil || sizeMM <= 0 {
		return TextItem{}, errors.New("generator: a text element has no font-size")
	}
	item := TextItem{
		Box:        box,
		FontKey:    attr(el, "data-font"),
		FontFamily: attr(el, "font-family"),
		// Two decimals: the SVG rounds the size in mm, this undoes it for whole-point sizes.
		FontSizePt: math.Round(sizeMM/mmPerPt*100) / 100,
		Color:      parseColor(attr(el, "fill")),
		AutoScale:  attr(el, "data-auto-scale") == "true",
	}
	if w, err := strconv.Atoi(attr(el, "font-weight")); err == nil {
		item.Bold = w >= 600
	} else {
		item.Bold = attr(el, "font-weight") == "bold"
	}
	switch attr(el, "text-anchor") {
	case "middle":
		item.Align = "center"
	case "end":
		item.Align = "right"
	default:
		item.Align = "left"
	}
	switch attr(el, "dominant-baseline") {
	case "text-before-edge":
		item.VAlign = "top"
	case "text-after-edge":
		item.VAlign = "bottom"
	default:
		item.VAlign = "middle"
	}

	// Content: either direct text (one line) or one <tspan> per line.
	var direct strings.Builder
	var lines []string
	var current *strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return TextItem{}, fmt.Errorf("generator: template is not valid SVG: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "tspan" && current == nil {
				current = &strings.Builder{}
			}
		case xml.EndElement:
			depth--
			if t.Name.Local == "tspan" && current != nil && depth == 1 {
				lines = append(lines, normalizeLine(current.String()))
				current = nil
			}
		case xml.CharData:
			if current != nil {
				current.Write(t)
			} else {
				direct.Write(t)
			}
		}
	}
	if len(lines) == 0 {
		lines = []string{normalizeLine(direct.String())}
	}
	item.Lines = lines
	return item, nil
}
