package generator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	if got, err := normalizeName("  BIB Default  "); err != nil || got != "BIB Default" {
		t.Errorf("trim: got %q, %v", got, err)
	}
	if _, err := normalizeName("   "); err == nil || err.Field != "name" {
		t.Errorf("blank name should be rejected on field name, got %v", err)
	}
	if _, err := normalizeName(strings.Repeat("x", maxNameLen+1)); err == nil {
		t.Error("over-long name should be rejected")
	}
	if _, err := normalizeName(strings.Repeat("é", maxNameLen)); err != nil {
		t.Errorf("the limit counts characters, not bytes: %v", err)
	}
}

func TestNormalizeMetadata(t *testing.T) {
	for _, raw := range []string{"", "null", "  "} {
		got, err := normalizeMetadata(json.RawMessage(raw))
		if err != nil || string(got) != "{}" {
			t.Errorf("%q: got %s, %v; want {}", raw, got, err)
		}
	}
	if got, err := normalizeMetadata(json.RawMessage(`{"width_mm":200}`)); err != nil || string(got) != `{"width_mm":200}` {
		t.Errorf("object kept as is: got %s, %v", got, err)
	}
	for _, raw := range []string{`[1]`, `"x"`, `12`, `{bad`} {
		if _, err := normalizeMetadata(json.RawMessage(raw)); err == nil {
			t.Errorf("%s should be rejected", raw)
		}
	}
	big := `{"a":"` + strings.Repeat("x", maxMetadataLen) + `"}`
	if _, err := normalizeMetadata(json.RawMessage(big)); err == nil {
		t.Error("oversized metadata should be rejected")
	}
}

func TestCheckObject(t *testing.T) {
	ok := ObjectRef{Bucket: "private", ContentType: "image/svg+xml", Status: "stored"}
	if err := checkObject(&ok); err != nil {
		t.Errorf("a stored private SVG is fine: %v", err)
	}
	for name, mutate := range map[string]func(*ObjectRef){
		"pending":    func(o *ObjectRef) { o.Status = "pending" },
		"public":     func(o *ObjectRef) { o.Bucket = "public" },
		"not an svg": func(o *ObjectRef) { o.ContentType = "image/png" },
	} {
		o := ok
		mutate(&o)
		if err := checkObject(&o); err == nil || err.Field != "storage_id" {
			t.Errorf("%s should be rejected on storage_id, got %v", name, err)
		}
	}
}

func TestKindValid(t *testing.T) {
	if !KindBIB.Valid() || !KindCertificate.Valid() || Kind("photo").Valid() || Kind("").Valid() {
		t.Error("only bib and certificate are valid kinds")
	}
}
