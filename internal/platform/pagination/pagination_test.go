package pagination

import (
	"testing"
	"time"
)

func TestPageParamsNormalizeLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero uses default", 0, DefaultPageLimit},
		{"negative uses default", -5, DefaultPageLimit},
		{"within range is kept", 7, 7},
		{"above max is clamped", 10_000, MaxPageLimit},
		{"exactly max is kept", MaxPageLimit, MaxPageLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PageParams{Limit: tc.in}.NormalizeLimit()
			if got != tc.want {
				t.Fatalf("NormalizeLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	want := time.Date(2026, 3, 14, 15, 9, 26, 535_897_932, time.UTC)
	id := "a1b2c3d4-0000-0000-0000-000000000001"

	encoded := EncodeCursor(want, id)
	if encoded == "" {
		t.Fatal("EncodeCursor returned empty string")
	}

	got, ok := DecodeCursor(encoded)
	if !ok {
		t.Fatal("DecodeCursor reported failure on a value it just encoded")
	}
	if !got.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want)
	}
	if got.ID != id {
		t.Errorf("id = %q, want %q", got.ID, id)
	}
}

func TestDecodeCursorDegradesGracefully(t *testing.T) {
	// An empty, garbage, or hand-edited cursor must never error the
	// caller - it should be treated as "start from the beginning" so a
	// stale/malformed client-supplied cursor never 400s the request.
	for _, raw := range []string{"", "not-base64!!!", "aGVsbG8", "|||"} {
		if _, ok := DecodeCursor(raw); ok {
			t.Errorf("DecodeCursor(%q) reported ok=true for invalid input", raw)
		}
	}
}
