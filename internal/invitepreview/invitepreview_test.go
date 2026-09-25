package invitepreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
)

func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"rani@example.com": "r***@example.com",
		"a@b.co":           "a***@b.co",
		"noatsign":         "***",
		"@x.com":           "***",
		"éva@x.com":        "é***@x.com",
	}
	for in, want := range cases {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveStatus(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	if got := ResolveStatus(StatusPending, past, now); got != StatusExpired {
		t.Errorf("pending past expiry = %q, want expired", got)
	}
	if got := ResolveStatus(StatusPending, future, now); got != StatusPending {
		t.Errorf("pending before expiry = %q, want pending", got)
	}
	if got := ResolveStatus(StatusRevoked, future, now); got != StatusRevoked {
		t.Errorf("revoked = %q, want revoked", got)
	}
}

type fakeSource struct {
	preview *Preview
	err     error
}

func (f fakeSource) PreviewInvitation(context.Context, string) (*Preview, error) {
	return f.preview, f.err
}

func get(h http.HandlerFunc, url string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w
}

func TestHandler(t *testing.T) {
	workspace := fakeSource{err: domain.ErrNotFound}
	event := fakeSource{preview: &Preview{
		Kind: KindEvent, Name: "Jogja Marathon", EventSlug: "jogja", Label: "volunteer",
		Capabilities: []string{"rpc:checkin"}, Email: "rani@example.com", Status: StatusPending,
	}}

	t.Run("falls through to the source that knows the token, masking the email", func(t *testing.T) {
		w := get(handler([]Source{workspace, event}), "/?token=abc")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", w.Code, w.Body)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"email_hint":"r***@example.com"`) {
			t.Errorf("email not masked: %s", body)
		}
		if strings.Contains(body, "rani@example.com") {
			t.Errorf("full email leaked: %s", body)
		}
	})
	t.Run("unknown token is 404", func(t *testing.T) {
		if w := get(handler([]Source{workspace, workspace}), "/?token=zzz"); w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
	t.Run("missing token is 400", func(t *testing.T) {
		if w := get(handler([]Source{workspace}), "/"); w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}
