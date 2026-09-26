package generator

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"rsc.io/qr"
)

// Certificate images: one JPG or PNG per participant, drawn like a BIB sheet
// but onto a bitmap. What is static (the design, images) comes from the PNG
// layers the browser drew when the template was saved; text and QR codes are
// drawn here from the tag values the request carries. The layout follows the
// PDF renderer (renderer.go) so both read a template the same way.

// ImageInput is one certificate.
type ImageInput struct {
	Artwork *Artwork
	// Rasters holds one PNG per static run, in stacking order (Artwork.StaticRuns).
	Rasters [][]byte
	// Values is what each tag prints. A tag that is missing prints as written
	// in the template; one that is present but empty prints nothing.
	Values map[string]string
	DPI    int
	Format string // "jpg" | "png"
	// Fonts defaults to the bundled fonts.
	Fonts FontProvider
}

// ImageResult is the encoded image and its size in pixels.
type ImageResult struct {
	Data              []byte
	WidthPx, HeightPx int
}

// jpegQuality matches the dashboard's JPG export (0.9).
const jpegQuality = 90

// ImageRenderer draws certificates of one template: it decodes the static
// layers and loads fonts once, then draws any number of images.
type ImageRenderer struct {
	art    *Artwork
	layers []image.Image
	fonts  FontProvider
	parsed map[string]*loadedTTF
}

type loadedTTF struct {
	font    *opentype.Font
	metrics FontMetrics
}

// NewImageRenderer checks the template can be drawn and prepares it.
func NewImageRenderer(art *Artwork, rasters [][]byte, fonts FontProvider) (*ImageRenderer, error) {
	if art == nil {
		return nil, errors.New("generator: no template to render")
	}
	if art.HasEmbeddedTags() {
		return nil, ErrEmbeddedTags
	}
	if runs := art.StaticRuns(); len(rasters) != runs {
		return nil, fmt.Errorf("%w (need %d layers, got %d)", ErrMissingRaster, runs, len(rasters))
	}
	if fonts == nil {
		fonts = BundledFonts{}
	}
	r := &ImageRenderer{art: art, fonts: fonts, parsed: map[string]*loadedTTF{}}
	for _, data := range rasters {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("generator: bad raster layer: %w", err)
		}
		r.layers = append(r.layers, img)
	}
	return r, nil
}

// PixelSize is the image size of the template at dpi (the dashboard's pixelSize).
func (r *ImageRenderer) PixelSize(dpi int) (w, h int) {
	return int(math.Round(r.art.WidthMM / 25.4 * float64(dpi))), int(math.Round(r.art.HeightMM / 25.4 * float64(dpi)))
}

func (r *ImageRenderer) font(key string, style FontStyle) (*loadedTTF, error) {
	id := key + "|" + style.Name()
	if f, ok := r.parsed[id]; ok {
		return f, nil
	}
	ttf, err := r.fonts.TTF(key, style)
	if err != nil {
		return nil, err
	}
	metrics, err := ReadMetrics(ttf)
	if err != nil {
		return nil, err
	}
	parsed, err := opentype.Parse(ttf)
	if err != nil {
		return nil, fmt.Errorf("generator: font %s: %w", key, err)
	}
	f := &loadedTTF{font: parsed, metrics: metrics}
	r.parsed[id] = f
	return f, nil
}

// Render draws one certificate.
func (r *ImageRenderer) Render(in ImageInput) (*ImageResult, error) {
	if in.DPI < 36 || in.DPI > 1200 {
		return nil, errors.New("generator: dpi out of range")
	}
	if in.Format != "jpg" && in.Format != "png" {
		return nil, errors.New("generator: format must be jpg or png")
	}
	w, h := r.PixelSize(in.DPI)
	if w < 1 || h < 1 || w*h > 200_000_000 {
		return nil, errors.New("generator: image size out of range")
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	pxPerMM := float64(in.DPI) / 25.4
	run, inRun := -1, false
	for _, item := range r.art.Items {
		switch it := item.(type) {
		case StaticItem:
			if !inRun {
				run++
				inRun = true
				draw.CatmullRom.Scale(canvas, canvas.Bounds(), r.layers[run], r.layers[run].Bounds(), draw.Over, nil)
			}
		case QRItem:
			inRun = false
			drawQRImage(canvas, it.Box, in.Values[it.Tag], pxPerMM)
		case TextItem:
			inRun = false
			if err := r.drawText(canvas, it, in.Values, in.DPI); err != nil {
				return nil, err
			}
		}
	}

	var buf bytes.Buffer
	var err error
	if in.Format == "png" {
		err = png.Encode(&buf, canvas)
	} else {
		err = jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return nil, err
	}
	return &ImageResult{Data: buf.Bytes(), WidthPx: w, HeightPx: h}, nil
}

// FillValues replaces every {{TAG}} of text with its value from values; a tag
// that has no value is left as written.
func FillValues(text string, values map[string]string) string {
	return TagPattern.ReplaceAllStringFunc(text, func(match string) string {
		key := strings.ToUpper(TagPattern.FindStringSubmatch(match)[1])
		if v, ok := values[key]; ok {
			return v
		}
		return match
	})
}

// drawText places a tag or text element like the PDF renderer does: anchored
// by its alignment inside its box, lines 1.2 font sizes apart, baselines put
// by the font's ascent and descent.
func (r *ImageRenderer) drawText(canvas *image.NRGBA, it TextItem, values map[string]string, dpi int) error {
	lines := strings.Split(FillValues(strings.Join(it.Lines, "\n"), values), "\n")
	key, ok := ResolveFontKey(it.FontKey, it.FontFamily)
	if !ok {
		return fmt.Errorf("%w: %q", ErrFontUnknown, FamilyName(it.FontFamily))
	}
	loaded, err := r.font(key, FontStyle{Bold: it.Bold, Italic: it.Italic})
	if err != nil {
		return err
	}
	pxPerMM := float64(dpi) / 25.4

	faceAt := func(sizePt float64) (font.Face, error) {
		return opentype.NewFace(loaded.font, &opentype.FaceOptions{Size: sizePt, DPI: float64(dpi), Hinting: font.HintingNone})
	}
	// Widths in mm at a size.
	measure := func(face font.Face) ([]float64, float64) {
		out, widest := make([]float64, len(lines)), 0.0
		for i, line := range lines {
			w := float64(font.MeasureString(face, line)) / 64 / pxPerMM
			out[i] = w
			widest = max(widest, w)
		}
		return out, widest
	}

	size := it.FontSizePt
	face, err := faceAt(size)
	if err != nil {
		return err
	}
	widths, widest := measure(face)
	// Shrink to fit the box width when the value is too long.
	if it.AutoScale && widest > it.Box.W {
		size = max(MinFontPt, size*it.Box.W/widest)
		if face, err = faceAt(size); err != nil {
			return err
		}
		widths, _ = measure(face)
	}

	sizeMM := size * mmPerPt
	gap := sizeMM * TextLineHeight
	n := float64(len(lines))
	m := loaded.metrics

	var anchorX float64
	switch it.Align {
	case "center":
		anchorX = it.Box.X + it.Box.W/2
	case "right":
		anchorX = it.Box.X + it.Box.W
	default:
		anchorX = it.Box.X
	}
	var firstY, baselineShift float64
	switch it.VAlign {
	case "top":
		firstY = it.Box.Y
		baselineShift = m.Ascent * sizeMM
	case "bottom":
		firstY = it.Box.Y + it.Box.H - (n-1)*gap
		baselineShift = -m.Descent * sizeMM
	default:
		firstY = it.Box.Y + it.Box.H/2 - (n-1)*gap/2
		baselineShift = (m.Ascent - m.Descent) / 2 * sizeMM
	}

	d := &font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(color.NRGBA{R: it.Color.R, G: it.Color.G, B: it.Color.B, A: 255}),
		Face: face,
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		x := anchorX
		switch it.Align {
		case "center":
			x -= widths[i] / 2
		case "right":
			x -= widths[i]
		}
		d.Dot = fixed.Point26_6{
			X: fixed.Int26_6(math.Round(x * pxPerMM * 64)),
			Y: fixed.Int26_6(math.Round((firstY + float64(i)*gap + baselineShift) * pxPerMM * 64)),
		}
		d.DrawString(line)
	}
	return nil
}

// drawQRImage draws value as a QR code of black squares, centered in the box.
func drawQRImage(canvas *image.NRGBA, box Box, value string, pxPerMM float64) {
	if value == "" {
		return
	}
	code, err := qr.Encode(value, qr.M)
	if err != nil {
		return
	}
	total := code.Size + 2*qrQuietModules
	side := min(box.W, box.H) * pxPerMM
	module := side / float64(total)
	originX := box.X*pxPerMM + (box.W*pxPerMM-side)/2 + qrQuietModules*module
	originY := box.Y*pxPerMM + (box.H*pxPerMM-side)/2 + qrQuietModules*module
	black := image.NewUniform(color.Black)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if !code.Black(x, y) {
				continue
			}
			rect := image.Rect(
				int(math.Round(originX+float64(x)*module)), int(math.Round(originY+float64(y)*module)),
				int(math.Round(originX+float64(x+1)*module)), int(math.Round(originY+float64(y+1)*module)),
			)
			draw.Draw(canvas, rect, black, image.Point{}, draw.Src)
		}
	}
}
