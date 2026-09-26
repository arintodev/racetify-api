package generator

import (
	"fmt"
	"math"
	"sort"
)

// Sheet layout for BIB print files. This is a port of the dashboard's
// lib/bib-print.ts (paper, grid, crop marks, print order); layout_test.go
// compares it against fixtures generated from that TypeScript, so the two
// cannot drift apart. All lengths are millimetres.

const (
	// SheetMarginMM is kept clear at every sheet edge for crop marks and the
	// printer's grip.
	SheetMarginMM = 10.0
	// CropMarkMM is the length of one crop mark.
	CropMarkMM = 5.0
	// PeoplePerFile: larger prints are split into several PDFs in one ZIP.
	PeoplePerFile = 500
)

// Size is a width and height in millimetres.
type Size struct {
	W float64 `json:"w"`
	H float64 `json:"h"`
}

var paperSizes = map[string]Size{
	"a4":     {W: 210, H: 297},
	"a3":     {W: 297, H: 420},
	"a3plus": {W: 329, H: 483},
	"sra3":   {W: 320, H: 450},
}

// Layout is how BIBs are laid out on a sheet.
type Layout struct {
	Paper       string  `json:"paper"`
	Orientation string  `json:"orientation"` // portrait | landscape
	Cols        int     `json:"cols"`
	Rows        int     `json:"rows"`
	CropMarks   bool    `json:"cropMarks"`
	GutterMM    float64 `json:"gutterMm"`
	BleedMM     float64 `json:"bleedMm"`
	// MarginMM is kept clear at every sheet edge; nil means SheetMarginMM.
	MarginMM *float64 `json:"marginMm,omitempty"`
}

// Margin is the room kept clear at every sheet edge.
func (l Layout) Margin() float64 {
	if l.MarginMM == nil {
		return SheetMarginMM
	}
	return *l.MarginMM
}

// PlacedCell is where one BIB's trim box sits on a sheet.
type PlacedCell struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// MarkLine is one crop mark.
type MarkLine struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
}

// SheetSize is the paper's size for an orientation.
func SheetSize(paper, orientation string) (Size, error) {
	size, ok := paperSizes[paper]
	if !ok {
		return Size{}, fmt.Errorf("generator: unknown paper %q", paper)
	}
	switch orientation {
	case "portrait":
		return size, nil
	case "landscape":
		return Size{W: size.H, H: size.W}, nil
	}
	return Size{}, fmt.Errorf("generator: unknown orientation %q", orientation)
}

// MaxGrid is how many columns and rows of BIBs (plus bleed on every side,
// gutter in between) fit inside the sheet margins.
func MaxGrid(cell Size, l Layout) (cols, rows int, err error) {
	sheet, err := SheetSize(l.Paper, l.Orientation)
	if err != nil {
		return 0, 0, err
	}
	fit := func(available, size float64) int {
		return int(math.Max(0, math.Floor((available+l.GutterMM)/(size+2*l.BleedMM+l.GutterMM))))
	}
	return fit(sheet.W-2*l.Margin(), cell.W), fit(sheet.H-2*l.Margin(), cell.H), nil
}

// Validate rejects a layout that is malformed or does not fit its paper.
func (l Layout) Validate(cell Size) error {
	if l.Cols < 1 || l.Rows < 1 {
		return fmt.Errorf("generator: layout needs at least 1 column and 1 row")
	}
	if l.GutterMM < 0 || l.BleedMM < 0 || l.GutterMM > 50 || l.BleedMM > 20 {
		return fmt.Errorf("generator: gutter or bleed out of range")
	}
	if l.Margin() < 0 || l.Margin() > 50 {
		return fmt.Errorf("generator: margin out of range")
	}
	cols, rows, err := MaxGrid(cell, l)
	if err != nil {
		return err
	}
	if l.Cols > cols || l.Rows > rows {
		return fmt.Errorf("generator: %dx%d BIBs do not fit %s (at most %dx%d)", l.Cols, l.Rows, l.Paper, cols, rows)
	}
	return nil
}

// PlaceCells returns the trim box of every cell on a sheet, the whole grid
// centered, row by row.
func PlaceCells(cell Size, l Layout) ([]PlacedCell, error) {
	sheet, err := SheetSize(l.Paper, l.Orientation)
	if err != nil {
		return nil, err
	}
	pitchX := cell.W + 2*l.BleedMM + l.GutterMM
	pitchY := cell.H + 2*l.BleedMM + l.GutterMM
	gridW := float64(l.Cols)*pitchX - l.GutterMM
	gridH := float64(l.Rows)*pitchY - l.GutterMM
	left := (sheet.W-gridW)/2 + l.BleedMM
	top := (sheet.H-gridH)/2 + l.BleedMM
	cells := make([]PlacedCell, 0, l.Cols*l.Rows)
	for row := 0; row < l.Rows; row++ {
		for col := 0; col < l.Cols; col++ {
			cells = append(cells, PlacedCell{
				X: left + float64(col)*pitchX, Y: top + float64(row)*pitchY, W: cell.W, H: cell.H,
			})
		}
	}
	return cells, nil
}

// CropMarkLines returns the crop marks along every cut line, outside the
// grid's outer bleed edge.
func CropMarkLines(cells []PlacedCell, bleed float64) []MarkLine {
	if len(cells) == 0 {
		return nil
	}
	offset := bleed + 1
	top, bottom := math.Inf(1), math.Inf(-1)
	left, right := math.Inf(1), math.Inf(-1)
	var xs, ys []float64
	seenX, seenY := map[float64]bool{}, map[float64]bool{}
	for _, c := range cells {
		top = math.Min(top, c.Y)
		bottom = math.Max(bottom, c.Y+c.H)
		left = math.Min(left, c.X)
		right = math.Max(right, c.X+c.W)
		for _, x := range []float64{c.X, c.X + c.W} {
			if !seenX[x] {
				seenX[x] = true
				xs = append(xs, x)
			}
		}
		for _, y := range []float64{c.Y, c.Y + c.H} {
			if !seenY[y] {
				seenY[y] = true
				ys = append(ys, y)
			}
		}
	}
	var lines []MarkLine
	for _, x := range xs {
		lines = append(lines,
			MarkLine{X1: x, X2: x, Y1: top - offset - CropMarkMM, Y2: top - offset},
			MarkLine{X1: x, X2: x, Y1: bottom + offset, Y2: bottom + offset + CropMarkMM},
		)
	}
	for _, y := range ys {
		lines = append(lines,
			MarkLine{Y1: y, Y2: y, X1: left - offset - CropMarkMM, X2: left - offset},
			MarkLine{Y1: y, Y2: y, X1: right + offset, X2: right + offset + CropMarkMM},
		)
	}
	return lines
}

// Person is what print order needs of a participant.
type Person struct {
	ID        string  `json:"id"`
	BibNumber int     `json:"bibNumber"`
	TeamID    *string `json:"teamId"`
	LegOrder  *int    `json:"legOrder"`
}

// PrintOrder sorts by BIB number, except that members of one team print next
// to each other (in leg order for relays), placed where the team's lowest BIB
// falls. The input is not modified.
func PrintOrder(people []Person) []Person {
	teamStart := map[string]int{}
	for _, p := range people {
		if p.TeamID == nil {
			continue
		}
		if cur, ok := teamStart[*p.TeamID]; !ok || p.BibNumber < cur {
			teamStart[*p.TeamID] = p.BibNumber
		}
	}
	group := func(p Person) int {
		if p.TeamID != nil {
			return teamStart[*p.TeamID]
		}
		return p.BibNumber
	}
	leg := func(p Person) int {
		if p.LegOrder != nil {
			return *p.LegOrder
		}
		return p.BibNumber
	}
	out := append([]Person(nil), people...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ga, gb := group(a), group(b); ga != gb {
			return ga < gb
		}
		if la, lb := leg(a), leg(b); la != lb {
			return la < lb
		}
		return a.BibNumber < b.BibNumber
	})
	return out
}

// Part is one PDF of a print: people [From, To) of the print order.
type Part struct{ From, To int }

// SplitParts divides n people into PeoplePerFile-sized parts.
func SplitParts(n int) []Part {
	var parts []Part
	for from := 0; from < n; from += PeoplePerFile {
		parts = append(parts, Part{From: from, To: min(from+PeoplePerFile, n)})
	}
	return parts
}
