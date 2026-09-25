// Package originpolicy decides which frontend origins the API may send a
// browser to or point a link at: the Google sign-in return URL and the
// accept links in invitation emails.
//
// A platform with one dashboard per tenant subdomain cannot list every host
// in an environment variable, so entries may be patterns:
//
//	https://app.racetify.com     exactly this origin
//	https://*.racetify.com       any single-label subdomain (lawu.racetify.com,
//	                             not a.b.racetify.com and not racetify.com)
//	http://localhost:3001        a port is part of the match
//
// The API never trusts an origin merely because a caller sent it: it is used
// only if it matches a configured entry, otherwise the default is used.
package originpolicy

import (
	"net/http"
	"net/url"
	"strings"
)

type pattern struct {
	scheme string
	// host is the exact host[:port], or, when wildcard is set, the suffix
	// after "*." (e.g. "racetify.com" or "localhost:3000").
	host     string
	wildcard bool
}

type Policy struct {
	patterns []pattern
	fallback string
}

// New builds a Policy from entries like those in the package doc. Malformed
// entries are ignored. The first non-wildcard entry becomes the default
// origin returned when a caller supplies none (or an unacceptable one).
func New(entries []string) *Policy {
	p := &Policy{}
	for _, raw := range entries {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		// net/url refuses '*' in a host, so lift the wildcard out first.
		wildcard := false
		if i := strings.Index(raw, "://*."); i >= 0 {
			wildcard = true
			raw = raw[:i+3] + raw[i+5:]
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.User != nil {
			continue
		}
		if strings.Contains(u.Host, "*") {
			continue
		}
		pt := pattern{scheme: u.Scheme, host: strings.ToLower(u.Host), wildcard: wildcard}
		p.patterns = append(p.patterns, pt)
		if p.fallback == "" && !pt.wildcard {
			p.fallback = pt.scheme + "://" + pt.host
		}
	}
	return p
}

// Default is the origin used when the caller's is missing or not allowed.
// Empty if no exact (non-wildcard) origin was configured.
func (p *Policy) Default() string { return p.fallback }

// Origin returns the scheme://host[:port] of rawURL when it is allowed.
func (p *Policy) Origin(rawURL string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Host)
	for _, pt := range p.patterns {
		if pt.scheme != u.Scheme {
			continue
		}
		if !pt.wildcard {
			if host == pt.host {
				return u.Scheme + "://" + host, true
			}
			continue
		}
		label, ok := strings.CutSuffix(host, "."+pt.host)
		if ok && label != "" && !strings.Contains(label, ".") {
			return u.Scheme + "://" + host, true
		}
	}
	return "", false
}

// Allowed reports whether rawURL's origin is allowed.
func (p *Policy) Allowed(rawURL string) bool {
	_, ok := p.Origin(rawURL)
	return ok
}

// LinkOrigin picks the origin for a link the API is about to email: the
// caller-supplied origin (typically the dashboard host the admin is using)
// if allowed, otherwise the default.
func (p *Policy) LinkOrigin(requested string) string {
	if origin, ok := p.Origin(requested); ok {
		return origin
	}
	return p.fallback
}

// RequestOrigin is the frontend origin a request claims to come from: an
// explicit X-Frontend-Origin (a server calling on a browser's behalf), else
// the Origin header a browser sends itself. It is only a claim; pass it
// through LinkOrigin, which checks it against the policy.
func RequestOrigin(r *http.Request) string {
	if o := r.Header.Get("X-Frontend-Origin"); o != "" {
		return o
	}
	return r.Header.Get("Origin")
}
