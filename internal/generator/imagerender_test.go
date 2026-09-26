package generator

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// A 100 x 50 mm certificate: a blue layer under a centered name and a QR code.
func testImageArtwork() *Artwork {
	return &Artwork{
		WidthMM: 100, HeightMM: 50,
		Items: []Item{
			StaticItem{Kind: "design"},
			TextItem{
				Box: Box{X: 10, Y: 10, W: 80, H: 10}, Lines: []string{"{{FULL_NAME}}"}, FontFamily: "Arial",
				FontSizePt: 20, Color: RGB{R: 255}, Align: "center", VAlign: "middle", AutoScale: true,
			},
			QRItem{Tag: "QR_CODE_VERIFY", Box: Box{X: 40, Y: 25, W: 20, H: 20}},
		},
	}
}

func bluePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 100, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.NRGBA{B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImageRendererDrawsLayerTextAndQR(t *testing.T) {
	r, err := NewImageRenderer(testImageArtwork(), [][]byte{bluePNG(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Render(ImageInput{
		DPI: 100, Format: "png",
		Values: map[string]string{"FULL_NAME": "Rizky Pratama", "QR_CODE_VERIFY": "JM26-7K4Q2X"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 100 mm x 50 mm at 100 dpi.
	if res.WidthPx != 394 || res.HeightPx != 197 {
		t.Fatalf("size %dx%d, want 394x197", res.WidthPx, res.HeightPx)
	}
	img, err := png.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	// The layer shows where nothing else is drawn.
	if r, g, b, _ := img.At(5, 5).RGBA(); r != 0 || g != 0 || b>>8 != 255 {
		t.Fatalf("top-left is not the blue layer: %d %d %d", r>>8, g>>8, b>>8)
	}
	// The name draws red pixels inside its box (10..20 mm down = 39..79 px).
	red := 0
	for y := 39; y < 79; y++ {
		for x := 39; x < 355; x++ {
			if r, g, b, _ := img.At(x, y).RGBA(); r>>8 > 200 && g>>8 < 80 && b>>8 < 80 {
				red++
			}
		}
	}
	if red < 50 {
		t.Fatalf("name not drawn: %d red pixels", red)
	}
	// The QR draws black pixels inside its box (25..45 mm down = 98..177 px).
	black := 0
	for y := 98; y < 177; y++ {
		for x := 157; x < 236; x++ {
			if r, g, b, _ := img.At(x, y).RGBA(); r == 0 && g == 0 && b == 0 {
				black++
			}
		}
	}
	if black < 200 {
		t.Fatalf("QR not drawn: %d black pixels", black)
	}
}

func TestImageRendererTextStaysCentered(t *testing.T) {
	r, err := NewImageRenderer(testImageArtwork(), [][]byte{bluePNG(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Render(ImageInput{DPI: 100, Format: "png", Values: map[string]string{"FULL_NAME": "Budi", "QR_CODE_VERIFY": ""}})
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(res.Data))
	minX, maxX := 1<<30, -1
	for y := 39; y < 79; y++ {
		for x := 0; x < 394; x++ {
			if r, g, b, _ := img.At(x, y).RGBA(); r>>8 > 200 && g>>8 < 80 && b>>8 < 80 {
				minX, maxX = min(minX, x), max(maxX, x)
			}
		}
	}
	if maxX < 0 {
		t.Fatal("name not drawn")
	}
	if centre := float64(minX+maxX) / 2; centre < 190 || centre > 204 {
		t.Fatalf("name centre at %.1f px, want about 197", centre)
	}
}

func TestImageRendererEncodesJPEGAndChecksInput(t *testing.T) {
	r, err := NewImageRenderer(testImageArtwork(), [][]byte{bluePNG(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Render(ImageInput{DPI: 150, Format: "jpg", Values: map[string]string{"FULL_NAME": "A"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(res.Data)); err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	if _, err := r.Render(ImageInput{DPI: 150, Format: "gif"}); err == nil {
		t.Fatal("a bad format was accepted")
	}
	if _, err := r.Render(ImageInput{DPI: 5, Format: "png"}); err == nil {
		t.Fatal("a bad dpi was accepted")
	}
	if _, err := NewImageRenderer(testImageArtwork(), nil, nil); err == nil {
		t.Fatal("a missing layer was accepted")
	}
}

func TestFillValues(t *testing.T) {
	values := map[string]string{"NET_TIME": "", "FULL_NAME": "Budi"}
	got := FillValues("{{FULL_NAME}} - {{net_time}} - {{UNKNOWN}}", values)
	if got != "Budi -  - {{UNKNOWN}}" {
		t.Fatalf("got %q", got)
	}
}

func TestItalicAndUnknownFonts(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="100mm" height="50mm" viewBox="0 0 100 50">` +
		`<text data-box="10 10 80 10" x="50" y="15" dominant-baseline="central" text-anchor="middle" ` +
		`font-family="Arial" font-size="7" font-weight="700" font-style="italic" fill="#000000">{{FULL_NAME}}</text>` +
		`<text data-box="10 30 80 10" x="50" y="35" dominant-baseline="central" text-anchor="middle" ` +
		`font-family="Bebas Neue" data-font="lib:abc" font-size="7" fill="#000000">{{CLUB}}</text></svg>`
	art, err := ParseTemplate([]byte(svg))
	if err != nil {
		t.Fatal(err)
	}
	first, second := art.Items[0].(TextItem), art.Items[1].(TextItem)
	if !first.Bold || !first.Italic {
		t.Errorf("first text: bold %v italic %v", first.Bold, first.Italic)
	}
	if second.Bold || second.Italic || second.FontKey != "lib:abc" {
		t.Errorf("second text: %+v", second)
	}

	// Bold italic Arial is drawn with the bundled bold-italic face.
	art.Items = art.Items[:1]
	r, err := NewImageRenderer(art, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(ImageInput{DPI: 100, Format: "png", Values: map[string]string{"FULL_NAME": "Budi"}}); err != nil {
		t.Fatalf("bold italic: %v", err)
	}

	// A family with no key is refused, not drawn in another font.
	art.Items = []Item{TextItem{
		Box: Box{X: 1, Y: 1, W: 50, H: 10}, Lines: []string{"x"}, FontFamily: "Bebas Neue", FontSizePt: 12,
		Align: "left", VAlign: "middle",
	}}
	r, _ = NewImageRenderer(art, nil, nil)
	if _, err := r.Render(ImageInput{DPI: 100, Format: "png"}); !errors.Is(err, ErrFontUnknown) {
		t.Errorf("unknown family: got %v", err)
	}
}
