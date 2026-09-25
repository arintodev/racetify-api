package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/racetify/racetify-api/internal/platform/originpolicy"
)

func TestCSRF(t *testing.T) {
	policy := originpolicy.New([]string{"https://app.racetify.com", "https://*.racetify.com"})
	h := CSRF(policy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	cookie := &http.Cookie{Name: AccessCookieName, Value: "jwt"}
	cases := []struct {
		name   string
		method string
		path   string
		origin string
		site   string
		cookie bool
		bearer bool
		want   int
	}{
		{"safe method never checked", http.MethodGet, "/api/v1/events", "https://evil.com", "", true, false, 200},
		{"cookie + allowed origin", http.MethodPost, "/api/v1/events", "https://app.racetify.com", "same-site", true, false, 200},
		{"cookie + tenant subdomain", http.MethodPost, "/api/v1/events", "https://lawu.racetify.com", "same-site", true, false, 200},
		{"cookie + foreign origin", http.MethodPost, "/api/v1/events", "https://evil.com", "cross-site", true, false, 403},
		{"cookie + no origin", http.MethodPost, "/api/v1/events", "", "", true, false, 403},
		{"cookie + cross-site fetch", http.MethodPost, "/api/v1/events", "https://app.racetify.com", "cross-site", true, false, 403},
		{"bearer needs no origin", http.MethodPost, "/api/v1/events", "", "", false, true, 200},
		{"bearer wins over a stray cookie", http.MethodPost, "/api/v1/events", "https://evil.com", "", true, true, 200},
		{"no credentials, not auth endpoint", http.MethodPost, "/api/v1/events", "", "", false, false, 200},
		{"login from foreign origin", http.MethodPost, "/api/v1/auth/login", "https://evil.com", "", false, false, 403},
		{"login from allowed origin", http.MethodPost, "/api/v1/auth/login", "https://app.racetify.com", "same-site", false, false, 200},
		{"login from a server", http.MethodPost, "/api/v1/auth/login", "", "", false, false, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.cookie {
				r.AddCookie(cookie)
			}
			if tc.bearer {
				r.Header.Set("Authorization", "Bearer x")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
}
