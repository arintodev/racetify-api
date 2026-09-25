package generator

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/signintech/gopdf"
	"rsc.io/qr"
)

// BIB print files (docs/bib-pdf-gopdf-plan.md). A sheet holds Layout.Cols ×
// Layout.Rows BIBs; each BIB is the template's stacking order drawn at its
// cell: raster layers for everything static (made by the browser, so the
// design looks exactly as previewed) and real vector text and QR codes for
// what changes per participant.

var (
	// ErrEmbeddedTags: the design holds {{TAG}} text of its own, which a raster
	// layer cannot fill per participant.
	ErrEmbeddedTags = errors.New("generator: the template design contains tags; make them tag elements in the editor")
	// ErrMissingRaster: the template needs a raster layer that was not supplied.
	ErrMissingRaster = errors.New("generator: a raster layer of the template is missing; save the template again")
)

// qrQuietModules is the blank margin around a QR code, in modules.
const qrQuietModules = 2

// cropMarkWidthMM is the 0.5 pt stroke the dashboard's PDF uses for crop marks.
const cropMarkWidthMM = 0.5 * mmPerPt

// RenderInput is one print.
type RenderInput struct {
	Artwork *Artwork
	// Rasters holds one PNG per static run, in stacking order (Artwork.StaticRuns).
	Rasters [][]byte
	// RasterBleedMM is how far each raster extends past the trim box on every side.
	RasterBleedMM float64
	Layout        Layout
	// Fonts defaults to the bundled fonts.
	Fonts FontProvider
	// People are the BIBs to print, already in print order (PrintOrder).
	People []TagContext
}

// File is one generated PDF.
type File struct {
	Name string
	Data []byte
}

// RenderResult is what a print produced.
type RenderResult struct {
	Files  []File
	Sheets int
}

// Render makes the print files: one PDF per PeoplePerFile people. progress, if
// set, is called with the number of BIBs done after each sheet. It stops early
// with ctx's error when ctx is cancelled.
func Render(ctx context.Context, in RenderInput, progress func(done int)) (*RenderResult, error) {
	if in.Artwork == nil {
		return nil, errors.New("generator: no template to render")
	}
	if len(in.People) == 0 {
		return nil, errors.New("generator: nobody to print")
	}
	art := in.Artwork
	cell := Size{W: art.WidthMM, H: art.HeightMM}
	if err := in.Layout.Validate(cell); err != nil {
		return nil, err
	}
	if art.HasEmbeddedTags() {
		return nil, ErrEmbeddedTags
	}
	if runs := art.StaticRuns(); len(in.Rasters) != runs {
		return nil, fmt.Errorf("%w (need %d layers, got %d)", ErrMissingRaster, runs, len(in.Rasters))
	}
	for _, item := range art.Items {
		if q, ok := item.(QRItem); ok && (!IsBIBTag(q.Tag) || !IsQRTag(q.Tag)) {
			return nil, fmt.Errorf("generator: %s is not a QR tag of a BIB", q.Tag)
		}
	}
	fonts := in.Fonts
	if fonts == nil {
		fonts = BundledFonts{}
	}
	sheet, err := SheetSize(in.Layout.Paper, in.Layout.Orientation)
	if err != nil {
		return nil, err
	}
	cells, err := PlaceCells(cell, in.Layout)
	if err != nil {
		return nil, err
	}
	var marks []MarkLine
	if in.Layout.CropMarks {
		marks = CropMarkLines(cells, in.Layout.BleedMM)
	}

	result := &RenderResult{}
	done := 0
	for n, part := range SplitParts(len(in.People)) {
		doc, err := newPDFDoc(sheet, fonts, in.Rasters)
		if err != nil {
			return nil, err
		}
		people := in.People[part.From:part.To]
		for start := 0; start < len(people); start += len(cells) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			doc.pdf.AddPage()
			for i, person := range people[start:min(start+len(cells), len(people))] {
				if err := doc.drawBIB(art, cells[i], in.RasterBleedMM, person); err != nil {
					return nil, err
				}
				done++
			}
			doc.drawMarks(marks)
			result.Sheets++
			if progress != nil {
				progress(done)
			}
		}
		result.Files = append(result.Files, File{
			Name: fmt.Sprintf("Part_%d_%d-%d.pdf", n+1, part.From+1, part.To),
			Data: doc.pdf.GetBytesPdf(),
		})
	}
	return result, nil
}

// Bundle packs a result for download: the PDF itself, or a ZIP when the print
// was split into several files.
func Bundle(files []File, baseName string) (name, contentType string, data []byte, err error) {
	if len(files) == 1 {
		return baseName + ".pdf", "application/pdf", files[0].Data, nil
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.Name)
		if err != nil {
			return "", "", nil, err
		}
		if _, err := w.Write(f.Data); err != nil {
			return "", "", nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return "", "", nil, err
	}
	return baseName + ".zip", "application/zip", buf.Bytes(), nil
}

// ==================== one PDF ====================

type loadedFont struct {
	name    string
	metrics FontMetrics
}

type pdfDoc struct {
	pdf     gopdf.GoPdf
	fonts   FontProvider
	loaded  map[string]loadedFont
	rasters []gopdf.ImageHolder
}

func newPDFDoc(sheet Size, fonts FontProvider, rasters [][]byte) (*pdfDoc, error) {
	d := &pdfDoc{fonts: fonts, loaded: map[string]loadedFont{}}
	d.pdf.Start(gopdf.Config{Unit: gopdf.UnitMM, PageSize: gopdf.Rect{W: sheet.W, H: sheet.H}})
	for _, png := range rasters {
		holder, err := gopdf.ImageHolderByBytes(png)
		if err != nil {
			return nil, fmt.Errorf("generator: bad raster layer: %w", err)
		}
		d.rasters = append(d.rasters, holder)
	}
	return d, nil
}

// useFont registers a font on first use and makes it current.
func (d *pdfDoc) useFont(key string, bold bool, sizePt float64) (FontMetrics, error) {
	id := fmt.Sprintf("%s|%t", key, bold)
	f, ok := d.loaded[id]
	if !ok {
		ttf, err := d.fonts.TTF(key, bold)
		if err != nil {
			return FontMetrics{}, err
		}
		metrics, err := ReadMetrics(ttf)
		if err != nil {
			return FontMetrics{}, err
		}
		if err := d.pdf.AddTTFFontData(id, ttf); err != nil {
			return FontMetrics{}, fmt.Errorf("generator: font %s: %w", key, err)
		}
		f = loadedFont{name: id, metrics: metrics}
		d.loaded[id] = f
	}
	if err := d.pdf.SetFont(f.name, "", sizePt); err != nil {
		return FontMetrics{}, err
	}
	return f.metrics, nil
}

// drawBIB draws one BIB with its trim box at cell.
func (d *pdfDoc) drawBIB(art *Artwork, cell PlacedCell, bleed float64, ctx TagContext) error {
	run, inRun := -1, false
	for _, item := range art.Items {
		switch it := item.(type) {
		case StaticItem:
			if !inRun {
				run++
				inRun = true
				err := d.pdf.ImageByHolder(d.rasters[run], cell.X-bleed, cell.Y-bleed,
					&gopdf.Rect{W: cell.W + 2*bleed, H: cell.H + 2*bleed})
				if err != nil {
					return fmt.Errorf("generator: drawing layer %d: %w", run, err)
				}
			}
		case QRItem:
			inRun = false
			if err := d.drawQR(cell, it.Box, TagValue(it.Tag, ctx)); err != nil {
				return err
			}
		case TextItem:
			inRun = false
			if err := d.drawText(cell, it, ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// drawText draws a tag or text element the way the browser draws the SVG that
// templateToSvg writes: anchored by its alignment inside its box, lines
// 1.2 font sizes apart, baselines placed by the font's ascent and descent.
func (d *pdfDoc) drawText(cell PlacedCell, it TextItem, ctx TagContext) error {
	text := FillTags(strings.Join(it.Lines, "\n"), ctx)
	lines := strings.Split(text, "\n")
	key := FontKeyFor(it.FontKey, it.FontFamily)

	size := it.FontSizePt
	metrics, err := d.useFont(key, it.Bold, size)
	if err != nil {
		return err
	}
	widths := func() ([]float64, float64, error) {
		out, widest := make([]float64, len(lines)), 0.0
		for i, line := range lines {
			w, err := d.pdf.MeasureTextWidth(line)
			if err != nil {
				return nil, 0, err
			}
			out[i] = w
			widest = max(widest, w)
		}
		return out, widest, nil
	}
	lineWidths, widest, err := widths()
	if err != nil {
		return err
	}
	// Shrink to fit the box width when the value is too long.
	if it.AutoScale && widest > it.Box.W {
		size = max(MinFontPt, size*it.Box.W/widest)
		if metrics, err = d.useFont(key, it.Bold, size); err != nil {
			return err
		}
		if lineWidths, _, err = widths(); err != nil {
			return err
		}
	}

	sizeMM := size * mmPerPt
	gap := sizeMM * TextLineHeight
	n := float64(len(lines))

	var anchorX float64
	switch it.Align {
	case "center":
		anchorX = it.Box.X + it.Box.W/2
	case "right":
		anchorX = it.Box.X + it.Box.W
	default:
		anchorX = it.Box.X
	}
	var anchorY, firstY, baselineShift float64
	switch it.VAlign {
	case "top":
		anchorY = it.Box.Y
		firstY = anchorY
		baselineShift = metrics.Ascent * sizeMM
	case "bottom":
		anchorY = it.Box.Y + it.Box.H
		firstY = anchorY - (n-1)*gap
		baselineShift = -metrics.Descent * sizeMM
	default:
		anchorY = it.Box.Y + it.Box.H/2
		firstY = anchorY - (n-1)*gap/2
		baselineShift = (metrics.Ascent - metrics.Descent) / 2 * sizeMM
	}

	d.pdf.SetTextColor(it.Color.R, it.Color.G, it.Color.B)
	for i, line := range lines {
		if line == "" {
			continue
		}
		x := anchorX
		switch it.Align {
		case "center":
			x -= lineWidths[i] / 2
		case "right":
			x -= lineWidths[i]
		}
		// gopdf puts the baseline of Text at the current Y.
		d.pdf.SetXY(cell.X+x, cell.Y+firstY+float64(i)*gap+baselineShift)
		if err := d.pdf.Text(line); err != nil {
			return err
		}
	}
	return nil
}

// drawQR draws value as a QR code of vector squares, centered in the box.
func (d *pdfDoc) drawQR(cell PlacedCell, box Box, value string) error {
	if value == "" {
		return nil
	}
	code, err := qr.Encode(value, qr.M)
	if err != nil {
		return fmt.Errorf("generator: QR code: %w", err)
	}
	total := code.Size + 2*qrQuietModules
	side := min(box.W, box.H)
	module := side / float64(total)
	originX := cell.X + box.X + (box.W-side)/2 + qrQuietModules*module
	originY := cell.Y + box.Y + (box.H-side)/2 + qrQuietModules*module

	d.pdf.SetFillColor(0, 0, 0)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; {
			if !code.Black(x, y) {
				x++
				continue
			}
			start := x
			for x < code.Size && code.Black(x, y) {
				x++
			}
			// One rectangle per horizontal run of dark modules.
			if err := d.pdf.Rectangle(
				originX+float64(start)*module, originY+float64(y)*module,
				originX+float64(x)*module, originY+float64(y+1)*module, "F", 0, 0,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *pdfDoc) drawMarks(marks []MarkLine) {
	if len(marks) == 0 {
		return
	}
	d.pdf.SetLineWidth(cropMarkWidthMM)
	d.pdf.SetStrokeColor(0, 0, 0)
	for _, m := range marks {
		d.pdf.Line(m.X1, m.Y1, m.X2, m.Y2)
	}
}
