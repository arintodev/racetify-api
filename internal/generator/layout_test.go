package generator

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

const eps = 1e-9

func near(a, b float64) bool { return math.Abs(a-b) <= eps }

type layoutFixtures struct {
	LayoutCases []struct {
		Cell    struct{ WidthMm, HeightMm float64 } `json:"cell"`
		Layout  Layout                              `json:"layout"`
		Sheet   struct{ WidthMm, HeightMm float64 } `json:"sheet"`
		MaxGrid struct{ Cols, Rows int }            `json:"maxGrid"`
		Cells   []PlacedCell                        `json:"cells"`
		Marks   []MarkLine                          `json:"marks"`
	} `json:"layoutCases"`
	OrderCases []struct {
		People []Person `json:"people"`
		Order  []string `json:"order"`
	} `json:"orderCases"`
}

func loadFixtures(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

// The expected values come from the dashboard's own lib/bib-print.ts.
func TestLayoutMatchesTypeScript(t *testing.T) {
	var fx layoutFixtures
	loadFixtures(t, "layout_fixtures.json", &fx)
	if len(fx.LayoutCases) < 50 {
		t.Fatalf("fixture too small: %d cases", len(fx.LayoutCases))
	}
	for i, tc := range fx.LayoutCases {
		cell := Size{W: tc.Cell.WidthMm, H: tc.Cell.HeightMm}

		sheet, err := SheetSize(tc.Layout.Paper, tc.Layout.Orientation)
		if err != nil || sheet.W != tc.Sheet.WidthMm || sheet.H != tc.Sheet.HeightMm {
			t.Fatalf("case %d: sheet %v (%v), want %v", i, sheet, err, tc.Sheet)
		}
		cols, rows, _ := MaxGrid(cell, tc.Layout)
		if cols != tc.MaxGrid.Cols || rows != tc.MaxGrid.Rows {
			t.Fatalf("case %d: max grid %dx%d, want %dx%d", i, cols, rows, tc.MaxGrid.Cols, tc.MaxGrid.Rows)
		}
		// The dashboard only offers grids that fit; anything larger must be refused.
		fits := tc.Layout.Cols <= cols && tc.Layout.Rows <= rows
		if err := tc.Layout.Validate(cell); (err == nil) != fits {
			t.Fatalf("case %d: fits=%v but Validate returned %v", i, fits, err)
		}

		cells, _ := PlaceCells(cell, tc.Layout)
		if len(cells) != len(tc.Cells) {
			t.Fatalf("case %d: %d cells, want %d", i, len(cells), len(tc.Cells))
		}
		for j, c := range cells {
			w := tc.Cells[j]
			if !near(c.X, w.X) || !near(c.Y, w.Y) || !near(c.W, w.W) || !near(c.H, w.H) {
				t.Fatalf("case %d cell %d: %+v, want %+v", i, j, c, w)
			}
		}

		marks := CropMarkLines(cells, tc.Layout.BleedMM)
		if len(marks) != len(tc.Marks) {
			t.Fatalf("case %d: %d marks, want %d", i, len(marks), len(tc.Marks))
		}
		for j, m := range marks {
			w := tc.Marks[j]
			if !near(m.X1, w.X1) || !near(m.Y1, w.Y1) || !near(m.X2, w.X2) || !near(m.Y2, w.Y2) {
				t.Fatalf("case %d mark %d: %+v, want %+v", i, j, m, w)
			}
		}
	}
}

func TestPrintOrderMatchesTypeScript(t *testing.T) {
	var fx layoutFixtures
	loadFixtures(t, "layout_fixtures.json", &fx)
	if len(fx.OrderCases) == 0 {
		t.Fatal("no order fixtures")
	}
	for i, tc := range fx.OrderCases {
		got := PrintOrder(tc.People)
		if len(got) != len(tc.Order) {
			t.Fatalf("case %d: %d people, want %d", i, len(got), len(tc.Order))
		}
		for j, p := range got {
			if p.ID != tc.Order[j] {
				t.Fatalf("case %d position %d: %s, want %s (whole order %v)", i, j, p.ID, tc.Order[j], tc.Order)
			}
		}
	}
}

func TestLayoutValidate(t *testing.T) {
	cell := Size{W: 200, H: 150}
	ok := Layout{Paper: "a4", Orientation: "landscape", Cols: 1, Rows: 1, GutterMM: 5, BleedMM: 3}
	if err := ok.Validate(cell); err != nil {
		t.Errorf("1x1 200x150 on A4 landscape should fit: %v", err)
	}
	if err := (Layout{Paper: "a4", Orientation: "portrait", Cols: 1, Rows: 1, GutterMM: 5, BleedMM: 3}).Validate(cell); err == nil {
		t.Error("200 mm wide plus bleed does not fit A4 portrait (190 mm usable)")
	}
	for name, l := range map[string]Layout{
		"too many":      {Paper: "a4", Orientation: "landscape", Cols: 3, Rows: 3, GutterMM: 5, BleedMM: 3},
		"zero cols":     {Paper: "a4", Orientation: "portrait", Cols: 0, Rows: 1},
		"unknown paper": {Paper: "letter", Orientation: "portrait", Cols: 1, Rows: 1},
		"bad orient":    {Paper: "a4", Orientation: "diagonal", Cols: 1, Rows: 1},
		"huge bleed":    {Paper: "a4", Orientation: "portrait", Cols: 1, Rows: 1, BleedMM: 99},
	} {
		if err := l.Validate(cell); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestSplitParts(t *testing.T) {
	cases := map[int][]Part{
		0:    nil,
		1:    {{0, 1}},
		500:  {{0, 500}},
		501:  {{0, 500}, {500, 501}},
		1200: {{0, 500}, {500, 1000}, {1000, 1200}},
	}
	for n, want := range cases {
		got := SplitParts(n)
		if len(got) != len(want) {
			t.Fatalf("n=%d: %v, want %v", n, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("n=%d: %v, want %v", n, got, want)
			}
		}
	}
}
