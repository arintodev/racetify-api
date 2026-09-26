package fontlib

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/image/font/sfnt"
)

// MaxFontBytes is the largest font file the library takes.
const MaxFontBytes = 5 << 20

// Info is what the library learns about a font file before keeping it.
type Info struct {
	Family    string
	Subfamily string
	// FSType is the OS/2 embedding-permission field.
	FSType uint16
	// Weight is the OS/2 usWeightClass (400 regular, 700 bold).
	Weight uint16
	// Bold and Italic are what the font says about itself (head.macStyle).
	Bold, Italic bool
	Glyphs       int
	// LatinMissing counts A-Z, a-z and 0-9 the font has no glyph for.
	LatinMissing int
}

// tables reads the table directory of an sfnt font.
func tables(data []byte) (map[string][]byte, error) {
	if len(data) < 12 {
		return nil, errors.New("the file is too short to be a font")
	}
	count := int(binary.BigEndian.Uint16(data[4:6]))
	out := make(map[string][]byte, count)
	for i := 0; i < count; i++ {
		entry := 12 + 16*i
		if entry+16 > len(data) {
			return nil, errors.New("the font's table directory is cut short")
		}
		offset := int(binary.BigEndian.Uint32(data[entry+8 : entry+12]))
		length := int(binary.BigEndian.Uint32(data[entry+12 : entry+16]))
		if offset < 0 || length < 0 || offset+length > len(data) {
			return nil, errors.New("a table of the font lies outside the file")
		}
		out[string(data[entry:entry+4])] = data[offset : offset+length]
	}
	return out, nil
}

// Inspect checks a font file can be embedded by the renderer and reads its
// details. The problems it returns are things the uploader can act on.
func Inspect(data []byte) (*Info, error) {
	if len(data) > MaxFontBytes {
		return nil, fmt.Errorf("the file is larger than %d MB", MaxFontBytes>>20)
	}
	if len(data) < 12 {
		return nil, errors.New("the file is too short to be a font")
	}
	switch string(data[0:4]) {
	case "wOFF", "wOF2":
		return nil, errors.New("WOFF fonts are not accepted: upload the TrueType (.ttf) file")
	case "OTTO":
		return nil, errors.New("OpenType fonts with CFF outlines are not accepted: upload a TrueType (.ttf) file")
	case "ttcf":
		return nil, errors.New("font collections (.ttc) are not accepted: upload one .ttf file")
	}
	if v := binary.BigEndian.Uint32(data[0:4]); v != 0x00010000 && string(data[0:4]) != "true" {
		return nil, errors.New("the file is not a TrueType font")
	}
	tbl, err := tables(data)
	if err != nil {
		return nil, err
	}
	if _, ok := tbl["fvar"]; ok {
		return nil, errors.New("variable fonts are not accepted: upload a static instance of the style")
	}
	if _, ok := tbl["glyf"]; !ok {
		return nil, errors.New("the font has no TrueType outlines (glyf table)")
	}
	head, os2 := tbl["head"], tbl["OS/2"]
	if len(head) < 46 || len(tbl["hhea"]) < 8 {
		return nil, errors.New("the font has no head/hhea table")
	}

	info := &Info{Weight: 400}
	if len(os2) >= 10 {
		info.Weight = binary.BigEndian.Uint16(os2[4:6])
		info.FSType = binary.BigEndian.Uint16(os2[8:10])
	}
	// fsType bit 1 (0x0002): restricted-license embedding, the font may not be embedded.
	if info.FSType&0x0002 != 0 {
		return nil, errors.New("the font's licence does not allow embedding (OS/2 fsType: restricted)")
	}
	macStyle := binary.BigEndian.Uint16(head[44:46])
	info.Bold, info.Italic = macStyle&1 != 0, macStyle&2 != 0

	f, err := sfnt.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("the font cannot be read: %v", err)
	}
	var buf sfnt.Buffer
	if info.Family, err = f.Name(&buf, sfnt.NameIDTypographicFamily); err != nil || info.Family == "" {
		if info.Family, err = f.Name(&buf, sfnt.NameIDFamily); err != nil || info.Family == "" {
			return nil, errors.New("the font has no family name")
		}
	}
	info.Subfamily, _ = f.Name(&buf, sfnt.NameIDSubfamily)
	info.Glyphs = f.NumGlyphs()
	if info.Glyphs == 0 {
		return nil, errors.New("the font has no glyphs")
	}
	for _, r := range latin {
		if idx, err := f.GlyphIndex(&buf, r); err != nil || idx == 0 {
			info.LatinMissing++
		}
	}
	if !utf8.ValidString(info.Family) {
		return nil, errors.New("the font's family name is not valid text")
	}
	return info, nil
}

var latin = func() []rune {
	var out []rune
	for r := 'A'; r <= 'Z'; r++ {
		out = append(out, r)
	}
	for r := 'a'; r <= 'z'; r++ {
		out = append(out, r)
	}
	for r := '0'; r <= '9'; r++ {
		out = append(out, r)
	}
	return out
}()
