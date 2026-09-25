package originpolicy

import "testing"

func TestPolicy(t *testing.T) {
	p := New([]string{"https://app.racetify.com", "https://*.racetify.com", "http://localhost:3001", " http://*.localhost:3000/ ", "bad", "https://*.*.x.com", "ftp://x.com"})

	allowed := []string{
		"https://app.racetify.com",
		"https://app.racetify.com/invite/abc?x=1",
		"https://lawu-trail.racetify.com",
		"HTTPS://Lawu-Trail.Racetify.com/path",
		"http://localhost:3001/cb",
		"http://lawu.localhost:3000",
	}
	denied := []string{
		"",
		"https://racetify.com",          // apex is not covered by *.racetify.com
		"https://a.b.racetify.com",      // one label only
		"https://racetify.com.evil.com", // suffix trick
		"https://evilracetify.com",      // no dot boundary
		"http://app.racetify.com",       // scheme mismatch
		"https://user@app.racetify.com", // userinfo
		"http://localhost:3002",         // port mismatch
		"javascript:alert(1)",
		"//app.racetify.com",
		"/relative",
	}
	for _, u := range allowed {
		if !p.Allowed(u) {
			t.Errorf("Allowed(%q) = false, want true", u)
		}
	}
	for _, u := range denied {
		if p.Allowed(u) {
			t.Errorf("Allowed(%q) = true, want false", u)
		}
	}
}

func TestLinkOrigin(t *testing.T) {
	p := New([]string{"https://app.racetify.com", "https://*.racetify.com"})
	if got := p.LinkOrigin("https://lawu.racetify.com"); got != "https://lawu.racetify.com" {
		t.Errorf("subdomain origin = %q", got)
	}
	if got := p.LinkOrigin("https://evil.com"); got != "https://app.racetify.com" {
		t.Errorf("disallowed origin should fall back to default, got %q", got)
	}
	if got := p.LinkOrigin(""); got != "https://app.racetify.com" {
		t.Errorf("empty origin should fall back to default, got %q", got)
	}
	if got := New([]string{"https://*.racetify.com"}).Default(); got != "" {
		t.Errorf("wildcard-only policy has no default, got %q", got)
	}
}
