package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/platform/originpolicy"
)

func TestLooksLikeEmail(t *testing.T) {
	valid := []string{"a@b.co", "first.last@example.com", "  padded@example.com  "}
	invalid := []string{"", "plain", "@example.com", "a@", "a@nodot", "a b@example.com", "a@b.c\nd"}
	for _, s := range valid {
		if !looksLikeEmail(s) {
			t.Errorf("looksLikeEmail(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if looksLikeEmail(s) {
			t.Errorf("looksLikeEmail(%q) = true, want false", s)
		}
	}
}

func TestReturnToAllowed(t *testing.T) {
	h := &Handler{origins: originpolicy.New([]string{"https://app.racetify.com", "https://*.racetify.com", "http://localhost:3001"})}
	allowed := []string{
		"https://app.racetify.com/login/google?next=%2Fteam&n=abc",
		"http://localhost:3001/login/google",
		"https://lawu-trail.racetify.com/login/google", // tenant subdomain, no per-host env entry
	}
	denied := []string{
		"",
		"/relative/path",
		"https://evil.com/login/google",
		"https://app.racetify.com.evil.com/cb",
		"https://user@app.racetify.com/cb", // userinfo trick
		"http://app.racetify.com/cb",       // scheme mismatch
		"javascript:alert(1)",
		"//app.racetify.com/cb",
	}
	for _, u := range allowed {
		if !h.returnToAllowed(u) {
			t.Errorf("returnToAllowed(%q) = false, want true", u)
		}
	}
	for _, u := range denied {
		if h.returnToAllowed(u) {
			t.Errorf("returnToAllowed(%q) = true, want false", u)
		}
	}
}

func TestReadRefreshToken(t *testing.T) {
	t.Run("body delivery reads JSON body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"refresh_token":"abc"}`))
		r.Header.Set("X-Token-Delivery", "body")
		w := httptest.NewRecorder()
		tok, ok := readRefreshToken(w, r)
		if !ok || tok != "abc" {
			t.Fatalf("got (%q, %v), want (abc, true)", tok, ok)
		}
	})
	t.Run("browser delivery reads cookie and ignores body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"refresh_token":"from-body"}`))
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "from-cookie"})
		w := httptest.NewRecorder()
		tok, ok := readRefreshToken(w, r)
		if !ok || tok != "from-cookie" {
			t.Fatalf("got (%q, %v), want (from-cookie, true)", tok, ok)
		}
	})
	t.Run("no token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		tok, ok := readRefreshToken(httptest.NewRecorder(), r)
		if !ok || tok != "" {
			t.Fatalf("got (%q, %v), want (\"\", true)", tok, ok)
		}
	})
}

func TestRespondSessionDelivery(t *testing.T) {
	h := &Handler{cfg: config.AuthConfig{CookieSecure: true, CookieDomain: ".racetify.com"}}
	user := &User{ID: "u1", Email: "a@b.co", FirstName: "A"}
	exp := time.Now().Add(time.Hour).UTC()
	tokens := &SessionTokens{AccessToken: "at", RefreshToken: "rt", AccessTokenExpiresAt: exp, RefreshTokenExpiresAt: exp}

	t.Run("browser gets cookies, no tokens in body", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.respondSession(w, httptest.NewRequest(http.MethodPost, "/", nil), http.StatusOK, user, tokens)

		got := map[string]*http.Cookie{}
		for _, c := range w.Result().Cookies() {
			got[c.Name] = c
		}
		access, refresh, hint := got[AccessCookieName], got[refreshCookieName], got[sessionHintCookieName]
		if access == nil || refresh == nil || hint == nil {
			t.Fatalf("want access, refresh and hint cookies, got %v", got)
		}
		if !access.HttpOnly || !refresh.HttpOnly || hint.HttpOnly {
			t.Errorf("HttpOnly wrong: access=%v refresh=%v hint=%v (hint must be readable)", access.HttpOnly, refresh.HttpOnly, hint.HttpOnly)
		}
		if access.Domain != "racetify.com" || !access.Secure || refresh.Path != "/api/v1/auth" {
			t.Errorf("cookie scope wrong: %+v / %+v", access, refresh)
		}
		if b := w.Body.String(); strings.Contains(b, `"access_token":`) || strings.Contains(b, `"refresh_token":`) {
			t.Fatalf("token leaked into body: %s", b)
		}
		payload, ok := decodeSessionHint(hint.Value)
		if !ok || payload.User.ID != "u1" || payload.RefreshExp != exp.Unix() || payload.AccessExp != exp.Unix() {
			t.Fatalf("hint payload wrong: %+v ok=%v", payload, ok)
		}
	})
	t.Run("body delivery gets tokens in body, no cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Token-Delivery", "body")
		w := httptest.NewRecorder()
		h.respondSession(w, r, http.StatusOK, user, tokens)
		if len(w.Result().Cookies()) != 0 {
			t.Fatalf("want no cookie, got %d", len(w.Result().Cookies()))
		}
		for _, want := range []string{`"refresh_token":"rt"`, `"access_token":"at"`} {
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("%s missing from body: %s", want, w.Body.String())
			}
		}
	})
}

func TestClearSessionCookies(t *testing.T) {
	h := &Handler{cfg: config.AuthConfig{CookieDomain: ".racetify.com"}}
	w := httptest.NewRecorder()
	h.clearSessionCookies(w)
	cookies := w.Result().Cookies()
	if len(cookies) != 3 {
		t.Fatalf("want 3 cleared cookies, got %d", len(cookies))
	}
	for _, c := range cookies {
		if c.MaxAge >= 0 || c.Value != "" {
			t.Errorf("%s not cleared: %+v", c.Name, c)
		}
	}
}

func TestNewRefreshTokenRecordClampsToAbsoluteCap(t *testing.T) {
	s := &Service{}
	s.cfg.RefreshTokenTTL = 7 * 24 * 3600 * 1e9

	soon := timeNowPlus(2) // absolute cap 2h away: sooner than the 7d idle window
	_, rec, err := s.newRefreshTokenRecord("u1", "fam", soon, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if !rec.ExpiresAt.Equal(soon) {
		t.Errorf("idle expiry %v not clamped to absolute cap %v", rec.ExpiresAt, soon)
	}
	if rec.FamilyID != "fam" || !rec.AbsoluteExpiresAt.Equal(soon) {
		t.Errorf("family/absolute not carried: %+v", rec)
	}
}

func timeNowPlus(hours int) time.Time {
	return time.Now().UTC().Add(time.Duration(hours) * time.Hour)
}
