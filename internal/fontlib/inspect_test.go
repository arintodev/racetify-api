package fontlib

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/racetify/racetify-api/internal/generator"
)

func bundledTTF(t *testing.T, style string) []byte {
	t.Helper()
	fam, _ := generator.BundledFamilyByKey(generator.FontLiberationSans)
	data, ok := fam.File(style)
	if !ok {
		t.Fatalf("no bundled %s", style)
	}
	return append([]byte(nil), data...)
}

func TestInspectReadsAValidFont(t *testing.T) {
	info, err := Inspect(bundledTTF(t, generator.StyleBold))
	if err != nil {
		t.Fatal(err)
	}
	if info.Family != "Liberation Sans" {
		t.Errorf("family %q", info.Family)
	}
	if !info.Bold || info.Italic || info.Weight < 600 {
		t.Errorf("style: %+v", info)
	}
	if info.Glyphs < 100 || info.LatinMissing != 0 {
		t.Errorf("glyphs %d, latin missing %d", info.Glyphs, info.LatinMissing)
	}
	italic, err := Inspect(bundledTTF(t, generator.StyleItalic))
	if err != nil || !italic.Italic || italic.Bold {
		t.Errorf("italic: %+v, %v", italic, err)
	}
}

func TestInspectRefusesWhatTheRendererCannotUse(t *testing.T) {
	ttf := bundledTTF(t, generator.StyleRegular)
	with := func(edit func([]byte) []byte) []byte { return edit(append([]byte(nil), ttf...)) }

	cases := map[string]struct {
		data []byte
		want string
	}{
		"woff":       {with(func(b []byte) []byte { copy(b, "wOFF"); return b }), "WOFF"},
		"woff2":      {with(func(b []byte) []byte { copy(b, "wOF2"); return b }), "WOFF"},
		"cff":        {with(func(b []byte) []byte { copy(b, "OTTO"); return b }), "CFF"},
		"collection": {with(func(b []byte) []byte { copy(b, "ttcf"); return b }), "collections"},
		"not a font": {[]byte("this is not a font at all"), "not a TrueType"},
		"too short":  {[]byte("abc"), "too short"},
		"too large":  {make([]byte, MaxFontBytes+1), "larger than"},
		"variable": {with(func(b []byte) []byte {
			// Rename a table to fvar: variable fonts carry one.
			renameTable(t, b, "gasp", "fvar")
			return b
		}), "variable"},
		"restricted licence": {with(func(b []byte) []byte {
			setFSType(t, b, 0x0002)
			return b
		}), "does not allow embedding"},
	}
	for name, tc := range cases {
		_, err := Inspect(tc.data)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error mentioning %q", name, err, tc.want)
		}
	}
}

func TestInspectAcceptsPermissiveEmbeddingFlags(t *testing.T) {
	for _, fsType := range []uint16{0x0000, 0x0004, 0x0008} {
		ttf := bundledTTF(t, generator.StyleRegular)
		setFSType(t, ttf, fsType)
		if _, err := Inspect(ttf); err != nil {
			t.Errorf("fsType %#x: %v", fsType, err)
		}
	}
}

// findTable returns the directory entry offset of a table.
func findTable(t *testing.T, data []byte, tag string) int {
	t.Helper()
	count := int(binary.BigEndian.Uint16(data[4:6]))
	for i := 0; i < count; i++ {
		entry := 12 + 16*i
		if string(data[entry:entry+4]) == tag {
			return entry
		}
	}
	t.Fatalf("no %s table", tag)
	return 0
}

func renameTable(t *testing.T, data []byte, from, to string) {
	t.Helper()
	copy(data[findTable(t, data, from):], to)
}

func setFSType(t *testing.T, data []byte, v uint16) {
	t.Helper()
	entry := findTable(t, data, "OS/2")
	offset := int(binary.BigEndian.Uint32(data[entry+8 : entry+12]))
	binary.BigEndian.PutUint16(data[offset+8:offset+10], v)
}

func TestCheckFileWarnsAboutAMismatchedStyle(t *testing.T) {
	_, warnings, err := checkFile(generator.StyleBold, bundledTTF(t, generator.StyleRegular))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "does not look bold") {
		t.Errorf("warnings %v", warnings)
	}
	if _, warnings, _ := checkFile(generator.StyleBold, bundledTTF(t, generator.StyleBold)); len(warnings) != 0 {
		t.Errorf("a matching style warned: %v", warnings)
	}
	if _, _, err := checkFile("heavy", bundledTTF(t, generator.StyleBold)); err == nil {
		t.Error("an unknown style was accepted")
	}
}

func TestSatisfiedByLibraryOrBundled(t *testing.T) {
	library := Font{ID: "abc", Family: "Roboto", Files: []FileInfo{{Style: generator.StyleBold}}}
	byKey := map[string]Font{"lib:abc": library}
	byName := map[string]Font{"roboto": library}
	cases := []struct {
		row  DemandRow
		want bool
	}{
		{DemandRow{Key: "lib:abc", Style: "bold"}, true},
		{DemandRow{Key: "lib:abc", Style: "regular"}, false},
		{DemandRow{Key: "lib:gone", Style: "bold"}, false},
		{DemandRow{Key: "bundled:liberation-sans", Style: "italic"}, true},
		{DemandRow{Key: "bundled:nope", Style: "regular"}, false},
		// No key, but the library has the name and style now: saving the template again fixes it.
		{DemandRow{Family: "Roboto", Style: "bold"}, true},
		{DemandRow{Family: "Roboto", Style: "regular"}, false},
		{DemandRow{Family: "Bebas Neue", Style: "regular"}, false},
	}
	for _, tc := range cases {
		if got := satisfied(tc.row, byKey, byName); got != tc.want {
			t.Errorf("%+v: got %v, want %v", tc.row, got, tc.want)
		}
	}
}

func TestBundledFileServing(t *testing.T) {
	data := bundledTTF(t, generator.StyleRegular)
	if digest(data) != digest(bytes.Clone(data)) || len(digest(data)) != 64 {
		t.Error("digest is not a stable sha256")
	}
}
