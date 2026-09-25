package generator

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	maxNameLen     = 120
	maxMetadataLen = 64 << 10
)

// normalizeName trims a template name and checks its length.
func normalizeName(name string) (string, *Error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", invalid("name", "name is required.")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", invalid("name", "name is too long.")
	}
	return name, nil
}

// normalizeMetadata makes sure metadata is a JSON object of sane size;
// nothing at all means an empty object.
func normalizeMetadata(raw json.RawMessage) (json.RawMessage, *Error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return json.RawMessage("{}"), nil
	}
	if len(raw) > maxMetadataLen {
		return nil, invalid("metadata", "metadata is too large.")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, invalid("metadata", "metadata must be a JSON object.")
	}
	return raw, nil
}

// checkObject decides whether an uploaded object can be a template's SVG: a
// finished upload of an SVG in the private bucket.
func checkObject(o *ObjectRef) *Error {
	if o.Status != "stored" {
		return invalid("storage_id", "the upload has not been completed yet.")
	}
	if o.Bucket != "private" {
		return invalid("storage_id", "a template must be stored in the private bucket.")
	}
	if !strings.HasPrefix(strings.ToLower(o.ContentType), "image/svg") {
		return invalid("storage_id", "a template must be an SVG file.")
	}
	return nil
}
