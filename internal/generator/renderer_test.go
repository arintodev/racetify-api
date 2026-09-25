package generator

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---------- helpers ----------

func testArtwork(t *testing.T, withDesign bool) *Artwork {
	t.Helper()
	var fx svgFixtures
	loadFixtures(t, "svg_fixtures.json", &fx)
	svg := fx.SVG
	if withDesign {
		// A design layer under everything, as the editor writes it.
		i := strings.Index(svg, ">") + 1
		svg = svg[:i] + `<svg x="0" y="0" width="200" height="150" viewBox="0 0 200 150"><rect width="200" height="150" fill="#ffe"/></svg>` + svg[i:]
	}
	art, err := ParseTemplate([]byte(svg))
	if err != nil {
		t.Fatal(err)
	}
	return art
}

// noisePNG is an opaque image that does not compress, so repeated embedding
// would show up in the file size.
func noisePNG(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(7))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func testPerson(bib int) TagContext {
	last := "Ayu Kusuma Dewi Pratiwi Sekar Wangi"
	club := "Jogja Runners"
	leg := 2
	return TagContext{
		Participant: Participant{BibNumber: bib, FirstName: "Ni Luh", LastName: &last, Club: &club, LegOrder: &leg, Gender: "female"},
		EventName:   "Bali 10K",
	}
}

var landscapeOne = Layout{Paper: "a4", Orientation: "landscape", Cols: 1, Rows: 1, CropMarks: true, GutterMM: 5, BleedMM: 3}

// pdfStreams returns every stream of a PDF, inflated when it is compressed.
func pdfStreams(pdf []byte) [][]byte {
	var out [][]byte
	for _, m := range regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`).FindAllSubmatch(pdf, -1) {
		zr, err := zlib.NewReader(bytes.NewReader(m[1]))
		if err != nil {
			out = append(out, m[1])
			continue
		}
		b, _ := io.ReadAll(zr)
		out = append(out, b)
	}
	return out
}

type textOp struct{ x, y, size float64 } // pt, PDF origin bottom-left

var textOpRe = regexp.MustCompile(`BT\s+(-?[\d.]+) (-?[\d.]+) TD\s+/F\d+ (-?[\d.]+) Tf`)

func textOps(pdf []byte) []textOp {
	var ops []textOp
	for _, s := range pdfStreams(pdf) {
		for _, m := range textOpRe.FindAllSubmatch(s, -1) {
			x, _ := strconv.ParseFloat(string(m[1]), 64)
			y, _ := strconv.ParseFloat(string(m[2]), 64)
			size, _ := strconv.ParseFloat(string(m[3]), 64)
			ops = append(ops, textOp{x, y, size})
		}
	}
	return ops
}

func mmToPt(mm float64) float64 { return mm / mmPerPt }

// measure is the width in mm of text set in a bundled font.
func measure(t *testing.T, key string, bold bool, sizePt float64, text string) float64 {
	t.Helper()
	d, err := newPDFDoc(Size{W: 100, H: 100}, BundledFonts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.useFont(key, bold, sizePt); err != nil {
		t.Fatal(err)
	}
	w, err := d.pdf.MeasureTextWidth(text)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func metricsOf(t *testing.T, key string, bold bool) FontMetrics {
	t.Helper()
	ttf, err := BundledFonts{}.TTF(key, bold)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadMetrics(ttf)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ---------- tests ----------

func TestFontMetricsAreThoseOfLiberationSans(t *testing.T) {
	m := metricsOf(t, FontLiberationSans, false)
	// Liberation Sans is metric-compatible with Arial: 0.905 / 0.212 em.
	if math.Abs(m.Ascent-0.905) > 0.002 || math.Abs(m.Descent-0.212) > 0.002 {
		t.Errorf("metrics %+v", m)
	}
}

func TestFontKeyFor(t *testing.T) {
	cases := map[string]string{
		"Arial, Helvetica, sans-serif":            FontLiberationSans,
		"'Arial Black', 'Arial Bold', sans-serif": FontLiberationSans,
		"Impact, 'Arial Narrow Bold', sans-serif": FontLiberationSans,
		"Verdana, Geneva, sans-serif":             FontDejaVuSans,
		"Georgia, 'Times New Roman', serif":       FontLiberationSerif,
		"'Courier New', Courier, monospace":       FontLiberationMono,
		"Comic Sans MS":                           DefaultFont,
		"":                                        DefaultFont,
	}
	for css, want := range cases {
		if got := FontKeyFor("", css); got != want {
			t.Errorf("FontKeyFor(%q) = %s, want %s", css, got, want)
		}
	}
	if got := FontKeyFor("tenant:abc", "Arial"); got != "tenant:abc" {
		t.Errorf("an explicit key must win, got %s", got)
	}
}

func TestRenderPlacesTextByTheBrowsersRules(t *testing.T) {
	art := testArtwork(t, false)
	res, err := Render(context.Background(), RenderInput{
		Artwork: art, Layout: landscapeOne, People: []TagContext{testPerson(3001)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Sheets != 1 || !bytes.HasPrefix(res.Files[0].Data, []byte("%PDF")) {
		t.Fatalf("unexpected result: %d files, %d sheets", len(res.Files), res.Sheets)
	}
	ops := textOps(res.Files[0].Data)
	// BIB number, full name, club, then the two lines of the static text.
	if len(ops) != 5 {
		t.Fatalf("%d text runs, want 5: %+v", len(ops), ops)
	}

	cell, _ := PlaceCells(Size{W: 200, H: 150}, landscapeOne)
	sheet, _ := SheetSize("a4", "landscape")
	pdfY := func(mm float64) float64 { return mmToPt(sheet.H - mm) }

	// 1. BIB number: 96 pt, centered, vertically middle (dominant-baseline central).
	bib := ops[0]
	sansBold := metricsOf(t, FontLiberationSans, true)
	sizeMM := 96.01 * mmPerPt
	wantBaseline := cell[0].Y + 30 + 60/2.0 + (sansBold.Ascent-sansBold.Descent)/2*sizeMM
	if math.Abs(bib.y-pdfY(wantBaseline)) > 0.05 || math.Abs(bib.size-96.01) > 0.02 {
		t.Errorf("BIB baseline %.2f pt size %.2f, want %.2f pt size 96.01", bib.y, bib.size, pdfY(wantBaseline))
	}
	w := measure(t, FontLiberationSans, true, 96.01, "3001")
	wantX := cell[0].X + 20 + 160/2.0 - w/2
	if math.Abs(bib.x-mmToPt(wantX)) > 0.05 {
		t.Errorf("BIB x %.2f pt, want %.2f (centered)", bib.x, mmToPt(wantX))
	}

	// 2. Full name in Georgia→Liberation Serif Bold, 28 pt, auto-scale: too long for 120 mm.
	name := "Ni Luh Ayu Kusuma Dewi Pratiwi Sekar Wangi"
	full := measure(t, FontLiberationSerif, true, 9.88/mmPerPt, name)
	if full <= 120 {
		t.Fatalf("test name must overflow its box, is %.1f mm", full)
	}
	wantSize := math.Round(9.88/mmPerPt*100) / 100 * 120 / full
	if math.Abs(ops[1].size-wantSize) > 0.05 {
		t.Errorf("auto-scaled size %.3f pt, want %.3f", ops[1].size, wantSize)
	}
	// left aligned, top: x = box left, baseline = box top + ascent
	if math.Abs(ops[1].x-mmToPt(cell[0].X+10)) > 0.05 {
		t.Errorf("left-aligned x %.2f", ops[1].x)
	}
	serif := metricsOf(t, FontLiberationSerif, true)
	if want := pdfY(cell[0].Y + 100 + serif.Ascent*wantSize*mmPerPt); math.Abs(ops[1].y-want) > 0.05 {
		t.Errorf("top-aligned baseline %.2f, want %.2f", ops[1].y, want)
	}

	// 3. Club: right aligned to the box's right edge, bottom baseline = box bottom - descent.
	club := measure(t, FontLiberationMono, false, 6.35/mmPerPt, "Jogja Runners")
	mono := metricsOf(t, FontLiberationMono, false)
	sz := math.Round(6.35/mmPerPt*100) / 100
	if want := mmToPt(cell[0].X + 190 - club*sz/(6.35/mmPerPt)); math.Abs(ops[2].x-want) > 0.1 {
		t.Errorf("right-aligned x %.2f, want %.2f", ops[2].x, want)
	}
	if want := pdfY(cell[0].Y + 132 - mono.Descent*sz*mmPerPt); math.Abs(ops[2].y-want) > 0.05 {
		t.Errorf("bottom-aligned baseline %.2f, want %.2f", ops[2].y, want)
	}

	// 4. Two-line static text: lines 1.2 font sizes apart, top aligned.
	gap := mmToPt(4.94 * TextLineHeight)
	if d := ops[3].y - ops[4].y; math.Abs(d-gap) > 0.1 {
		t.Errorf("line pitch %.2f pt, want %.2f", d, gap)
	}
}

func TestRenderEmbedsSharedRasterOnceAndDrawsQR(t *testing.T) {
	art := testArtwork(t, true)
	raster := noisePNG(400, 300)
	in := RenderInput{Artwork: art, Rasters: [][]byte{raster}, RasterBleedMM: 3, Layout: landscapeOne}
	for i := 0; i < 6; i++ {
		in.People = append(in.People, testPerson(3000+i))
	}
	var progress []int
	res, err := Render(context.Background(), in, func(done int) { progress = append(progress, done) })
	if err != nil {
		t.Fatal(err)
	}
	pdf := res.Files[0].Data
	if res.Sheets != 6 || len(progress) != 6 || progress[5] != 6 {
		t.Errorf("sheets=%d progress=%v", res.Sheets, progress)
	}
	if n := len(regexp.MustCompile(`/Type /Page\b`).FindAll(pdf, -1)); n != 6 {
		t.Errorf("%d page objects, want 6", n)
	}
	// Six pages, one embedded image: the file is about one raster, not six.
	if len(pdf) > 2*len(raster) {
		t.Errorf("file is %d bytes for a %d byte raster on 6 pages: the image is embedded more than once", len(pdf), len(raster))
	}
	if n := len(regexp.MustCompile(`/Subtype /Image`).FindAll(pdf, -1)); n != 1 {
		t.Errorf("%d image objects, want 1", n)
	}
	// The QR code is many filled squares (plus the crop marks' strokes).
	page := ""
	for _, s := range pdfStreams(pdf) {
		if regexp.MustCompile(`/I\d+ Do`).Match(s) {
			page = string(s)
			break
		}
	}
	if page == "" {
		t.Fatal("no page content stream found")
	}
	if fills := strings.Count(page, "\nf\n") + strings.Count(page, " f\n"); fills < 30 {
		t.Errorf("only %d fills: the QR code was not drawn", fills)
	}
}

func TestRenderSplitsIntoPartsAndZips(t *testing.T) {
	art := testArtwork(t, false)
	in := RenderInput{Artwork: art, Layout: Layout{Paper: "sra3", Orientation: "landscape", Cols: 2, Rows: 1, GutterMM: 5, BleedMM: 3}}
	for i := 0; i < PeoplePerFile+1; i++ {
		in.People = append(in.People, testPerson(1+i))
	}
	res, err := Render(context.Background(), in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || res.Files[0].Name != "Part_1_1-500.pdf" || res.Files[1].Name != "Part_2_501-501.pdf" {
		t.Fatalf("files: %v", []string{res.Files[0].Name, res.Files[len(res.Files)-1].Name})
	}
	if res.Sheets != 250+1 {
		t.Errorf("%d sheets, want 251 (500 BIB at 2 per sheet, then 1)", res.Sheets)
	}
	name, ctype, data, err := Bundle(res.Files, "bib-run")
	if err != nil || name != "bib-run.zip" || ctype != "application/zip" {
		t.Fatalf("bundle: %s %s %v", name, ctype, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("zip: %v, %d entries", err, len(zr.File))
	}
	one, ctype, _, _ := func() (string, string, []byte, error) { return Bundle(res.Files[:1], "x") }()
	if one != "x.pdf" || ctype != "application/pdf" {
		t.Errorf("a single file is delivered as a PDF, got %s %s", one, ctype)
	}
}

func TestRenderRefusesWhatItCannotPrint(t *testing.T) {
	people := []TagContext{testPerson(1)}
	art := testArtwork(t, false)
	ctx := context.Background()

	if _, err := Render(ctx, RenderInput{Artwork: art, Layout: landscapeOne}, nil); err == nil {
		t.Error("nobody to print must be refused")
	}
	if _, err := Render(ctx, RenderInput{Artwork: art, Layout: Layout{Paper: "a4", Orientation: "portrait", Cols: 1, Rows: 1, BleedMM: 3}, People: people}, nil); err == nil {
		t.Error("a layout that does not fit must be refused")
	}

	withDesign := testArtwork(t, true)
	if _, err := Render(ctx, RenderInput{Artwork: withDesign, Layout: landscapeOne, People: people}, nil); !errors.Is(err, ErrMissingRaster) {
		t.Errorf("missing raster: %v", err)
	}
	if _, err := Render(ctx, RenderInput{Artwork: withDesign, Rasters: [][]byte{[]byte("not a png")}, Layout: landscapeOne, People: people}, nil); err == nil {
		t.Error("a broken raster must be refused")
	}

	tagged, _ := ParseTemplate([]byte(`<svg width="100mm" height="70mm" viewBox="0 0 100 70"><svg><text>{{FULL_NAME}}</text></svg></svg>`))
	if _, err := Render(ctx, RenderInput{Artwork: tagged, Rasters: [][]byte{noisePNG(10, 10)}, Layout: landscapeOne, People: people}, nil); !errors.Is(err, ErrEmbeddedTags) {
		t.Errorf("embedded tags: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Render(cancelled, RenderInput{Artwork: art, Layout: landscapeOne, People: people}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled render: %v", err)
	}

	unknownFont := RenderInput{Artwork: art, Layout: landscapeOne, People: people, Fonts: noFonts{}}
	if _, err := Render(ctx, unknownFont, nil); err == nil {
		t.Error("a font the provider cannot supply must fail the render, not be replaced silently")
	}
}

type noFonts struct{}

func (noFonts) TTF(string, bool) ([]byte, error) { return nil, errors.New("no such font") }
