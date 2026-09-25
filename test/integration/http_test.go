//go:build integration

// Run with:
//
//	DATABASE_URL=postgres://app_user:app_user_dev_password@localhost:5432/racetify?sslmode=disable \
//	DATABASE_ADMIN_URL=postgres://platform_admin:platform_admin_dev_password@localhost:5432/racetify?sslmode=disable \
//	REDIS_ADDR=localhost:6379 \
//	go test -tags=integration ./test/integration/... -v
//
// (see Makefile's `test-integration` target, which sets all of this up
// including running migrations first).
//
// This is the HTTP-level companion to
// internal/repository/rls_isolation_test.go: it drives the real router
// (internal/httpapi.NewRouter) through httptest, so it proves the full
// stack - not just the RLS policy - behaves per Phase 0's deliverables:
// Runner registration/login, Tenant registration, the Staff invitation
// flow, RBAC, the OAuth 2.0 Client Credentials grant, and that neither a
// cross-tenant User session nor a cross-tenant M2M token can read another
// tenant's data.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/racetify/racetify-api/internal/app"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/httpapi"
	"github.com/racetify/racetify-api/internal/platform/logger"
)

func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set; skipping integration test (see file header for how to run it)", key)
	}
	return v
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWithLogger(t, logger.New("development"))
}

// newTestServerWithLogger is newTestServer's variant for a test that needs
// to observe a structured log line the app emits - specifically
// LogMailer's "link" field (internal/mailer/mailer.go), since, unlike an
// invitation token (dto.go's InvitationCreatedDTO comment explains why
// that one IS returned directly in its API response), the email-
// verification token is never returned in any API response at all.
// LogMailer's own doc comment calls this out as deliberate: "so the flows
// are fully exercisable (and testable) in this sandbox" - a caller-supplied
// logger is that mechanism. newTestServer above is a thin wrapper around
// this using the same plain stdout logger every other test gets.
func newTestServerWithLogger(t *testing.T, log *slog.Logger) *httptest.Server {
	t.Helper()

	os.Setenv("DATABASE_URL", mustEnv(t, "DATABASE_URL"))
	os.Setenv("DATABASE_ADMIN_URL", mustEnv(t, "DATABASE_ADMIN_URL"))
	os.Setenv("REDIS_ADDR", mustEnv(t, "REDIS_ADDR"))
	os.Setenv("JWT_SECRET", "integration-test-secret")
	os.Setenv("APP_ENV", "development")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	a, err := app.Build(ctx, cfg, log)
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	t.Cleanup(a.Close)

	srv := httptest.NewServer(httpapi.NewRouter(a.Handler))
	t.Cleanup(srv.Close)
	return srv
}

// clientIPCounter hands out a distinct synthetic source IP per client (see
// newClient) so that IP-keyed rate limiting (middleware.RateLimit on
// /api/v1/auth/login and /oauth/token) never shares a budget between
// unrelated clients/tests - every request in this whole package's test
// binary shares one Redis instance and, without this, would otherwise all
// report the same RemoteAddr (httptest.Server always sees 127.0.0.1), so a
// rate-limit test firing many requests from one client would silently eat
// into every other test's login budget too.
var clientIPCounter atomic.Int64

// client wraps http.Client with a cookie jar (for the refresh-token
// cookie) and small JSON helpers, so the test body below reads like the
// curl-based smoke test it mirrors.
type client struct {
	http   *http.Client
	base   string
	access string
	ip     string // synthetic X-Forwarded-For, unique per client - see clientIPCounter.
}

func newClient(base string) *client {
	jar, _ := cookiejar.New(nil)
	n := clientIPCounter.Add(1)
	// 203.0.113.0/24 is TEST-NET-3 (RFC 5737), reserved for documentation/
	// examples and never a real caller. This is only ever used as an opaque
	// rate-limit-key string via X-Forwarded-For (never parsed as a real IP
	// by middleware.ClientIP), but split the counter across the last two
	// octets anyway so it stays a well-formed IPv4 address up to 65536
	// clients in one test run - far more than any test package needs.
	ip := fmt.Sprintf("203.0.%d.%d", (n>>8)&0xff, n&0xff)
	return &client{http: &http.Client{Jar: jar, Timeout: 10 * time.Second}, base: base, ip: ip}
}

type apiResponse struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *client) do(t *testing.T, method, path string, headers map[string]string, body any) (int, apiResponse) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.access != "" {
		req.Header.Set("Authorization", "Bearer "+c.access)
	}
	if c.ip != "" {
		req.Header.Set("X-Forwarded-For", c.ip)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	var parsed apiResponse
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func (r apiResponse) field(t *testing.T, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatalf("decode data: %v (raw: %s)", err, string(r.Data))
	}
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("field %q missing or not a string in %s", key, string(r.Data))
	}
	return v
}

// registerAndLogin still takes a single display name for every call site's
// convenience; it splits on the first space into first_name/last_name (the
// same fallback shape as service.splitFullName) since the register endpoint
// itself takes the two fields separately.
func registerAndLogin(t *testing.T, c *client, email, name string) {
	t.Helper()
	firstName, lastName := name, ""
	if i := strings.IndexByte(name, ' '); i >= 0 {
		firstName, lastName = name[:i], name[i+1:]
	}
	status, resp := c.do(t, http.MethodPost, "/api/v1/auth/register", nil, map[string]string{
		"email": email, "password": "supersecret1", "first_name": firstName, "last_name": lastName,
	})
	if status != http.StatusCreated {
		t.Fatalf("register %s: status %d: %+v", email, status, resp)
	}

	status, resp = c.do(t, http.MethodPost, "/api/v1/auth/login", nil, map[string]string{
		"email": email, "password": "supersecret1",
	})
	if status != http.StatusOK {
		t.Fatalf("login %s: status %d: %+v", email, status, resp)
	}
	c.access = c.accessFromCookie(t)
}

// switchTenant calls POST /api/v1/auth/switch-tenant and, on success,
// replaces c.access with the returned tenant-scoped token - the
// replacement for what used to be an X-Tenant-ID header on every
// tenant-scoped call (see internal/httpapi/middleware/tenant.go's
// RequireTenantForUser doc comment). It does NOT fail the test itself on
// a non-200: several call sites below deliberately switchTenant into a
// tenant the caller is not a member of to prove the 403 that used to be
// asserted on the resource call itself now happens one step earlier, at
// selection time - see TestFullPhase0Flow's "Cross-tenant isolation"
// section.
func (c *client) switchTenant(t *testing.T, tenantID string) (int, apiResponse) {
	t.Helper()
	status, resp := c.do(t, http.MethodPost, "/api/v1/auth/switch-tenant", nil, map[string]string{"tenant_id": tenantID})
	if status == http.StatusOK {
		c.access = c.accessFromCookie(t)
	}
	return status, resp
}

// refresh calls POST /api/v1/auth/refresh (the refresh-token cookie
// travels automatically via c.http's jar) and, on success, replaces
// c.access with the returned access token - same shape as switchTenant.
// Because client.do already attaches c.access as the Authorization header
// on every request, this exercises RefreshAccessToken's
// previousAccessToken hint (Service.RefreshAccessToken's doc comment) for
// free: whatever tenant c.access was scoped to before this call is what
// the server tries to carry forward into the new one.
func (c *client) refresh(t *testing.T) (int, apiResponse) {
	t.Helper()
	status, resp := c.do(t, http.MethodPost, "/api/v1/auth/refresh", nil, nil)
	if status == http.StatusOK {
		c.access = c.accessFromCookie(t)
	}
	return status, resp
}

func TestFullPhase0Flow(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())

	ownerA := newClient(srv.URL)
	ownerB := newClient(srv.URL)

	registerAndLogin(t, ownerA, "owner-a-"+unique+"@example.com", "Owner A")
	registerAndLogin(t, ownerB, "owner-b-"+unique+"@example.com", "Owner B")

	// ---- Tenant registration ----
	status, resp := ownerA.do(t, http.MethodPost, "/api/v1/tenants", nil, map[string]string{"name": "Jakarta Marathon " + unique})
	if status != http.StatusCreated {
		t.Fatalf("create tenant A: status %d: %+v", status, resp)
	}
	tenantA := resp.field(t, "id")

	status, resp = ownerB.do(t, http.MethodPost, "/api/v1/tenants", nil, map[string]string{"name": "Bali Ultra " + unique})
	if status != http.StatusCreated {
		t.Fatalf("create tenant B: status %d: %+v", status, resp)
	}
	tenantB := resp.field(t, "id")

	// ---- Cross-tenant isolation: a User session ----
	// Under the header-based model this 403 came from GET .../summary
	// itself; now that tenant selection happens at token-issuance time
	// (POST /auth/switch-tenant), the very same membership check runs one
	// step earlier - owner A is never able to mint a tenant-B-scoped
	// access token to begin with.
	status, _ = ownerA.switchTenant(t, tenantB)
	if status != http.StatusForbidden {
		t.Fatalf("CROSS-TENANT LEAK (user session): owner A switched into tenant B, status=%d, want 403", status)
	}

	// ---- Same-tenant access works ----
	status, _ = ownerA.switchTenant(t, tenantA)
	if status != http.StatusOK {
		t.Fatalf("owner A switching into own tenant: status %d", status)
	}
	status, resp = ownerA.do(t, http.MethodGet, "/api/v1/tenant/summary", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("owner A reading own tenant summary: status %d: %+v", status, resp)
	}
	if got := resp.field(t, "tenant_id"); got != tenantA {
		t.Fatalf("tenant summary returned tenant_id=%s, want %s", got, tenantA)
	}

	// ---- Refresh carries the selected tenant forward on its own: the
	// still-live access token client.do sends as Authorization on this
	// call is read purely as a hint (TokenManager.ParseIgnoringExpiry),
	// with membership re-verified before it's trusted - see
	// Service.RefreshAccessToken's doc comment. No tenant_id is passed
	// explicitly here. ----
	status, resp = ownerA.refresh(t)
	if status != http.StatusOK {
		t.Fatalf("refresh: status %d: %+v", status, resp)
	}
	if got := resp.field(t, "tenant_id"); got != tenantA {
		t.Fatalf("refresh did not carry the selected tenant forward: tenant_id=%q, want %q", got, tenantA)
	}

	// ---- Staff invitation flow ----
	staffEmail := "staff-" + unique + "@example.com"
	status, resp = ownerA.do(t, http.MethodPost, "/api/v1/invitations",
		nil, map[string]string{"email": staffEmail, "role": "staff"})
	if status != http.StatusCreated {
		t.Fatalf("invite staff: status %d: %+v", status, resp)
	}

	// The invitation response includes the raw token (see
	// handlers.InvitationCreatedDTO's doc comment: the inviting admin is a
	// legitimate holder of it) - in production it is also emailed via
	// mailer.Mailer, but a test has no inbox to read from.
	rawToken := resp.field(t, "token")

	// ---- List endpoints are paginated: {"items": [...], "next_cursor": ...} ----
	// (internal/platform/pagination's keyset convention, surfaced
	// through internal/httpapi/handlers.ListResponseDTO). limit=1 against a
	// tenant with exactly one pending invitation exercises both the
	// envelope shape and the "no further pages" (absent next_cursor) case
	// end to end over real HTTP, not just at the repository layer (see
	// internal/repository/pagination_integration_test.go for the deeper
	// multi-page proof).
	status, resp = ownerA.do(t, http.MethodGet, "/api/v1/invitations?limit=1", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list invitations: status %d: %+v", status, resp)
	}
	var invPage struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(resp.Data, &invPage); err != nil {
		t.Fatalf("decode paginated invitations response: %v (raw: %s)", err, string(resp.Data))
	}
	if len(invPage.Items) != 1 {
		t.Fatalf("list invitations with limit=1: got %d items, want 1", len(invPage.Items))
	}
	if invPage.NextCursor != "" {
		t.Fatalf("list invitations: got next_cursor=%q with only one invitation total, want none", invPage.NextCursor)
	}

	staff := newClient(srv.URL)
	registerAndLogin(t, staff, staffEmail, "Staff One")

	status, resp = staff.do(t, http.MethodPost, "/api/v1/invitations/accept", nil, map[string]string{"token": rawToken})
	if status != http.StatusOK {
		t.Fatalf("accept invitation: status %d: %+v", status, resp)
	}

	// Membership now exists, so this must succeed - unlike ownerA's probe
	// above, this switch itself is not the thing under test here.
	status, _ = staff.switchTenant(t, tenantA)
	if status != http.StatusOK {
		t.Fatalf("staff switching into tenant A after accepting invitation: status %d", status)
	}

	// ---- RBAC: staff cannot provision M2M credentials (admin+ only) ----
	status, _ = staff.do(t, http.MethodPost, "/api/v1/oauth-clients", nil, map[string]string{"name": "x"})
	if status != http.StatusForbidden {
		t.Fatalf("staff creating oauth client: status=%d, want 403 (staff is below admin)", status)
	}

	// ---- Server-to-Server OAuth 2.0 Client Credentials grant ----
	status, resp = ownerA.do(t, http.MethodPost, "/api/v1/oauth-clients",
		nil, map[string]any{"name": "Timing System", "scopes": []string{"tenant:read"}})
	if status != http.StatusCreated {
		t.Fatalf("create oauth client: status %d: %+v", status, resp)
	}
	clientID := resp.field(t, "client_id")
	clientSecret := resp.field(t, "client_secret")

	status, resp = newClient(srv.URL).do(t, http.MethodPost, "/oauth/token", nil, map[string]string{
		"grant_type": "client_credentials", "client_id": clientID, "client_secret": clientSecret, "scope": "tenant:read",
	})
	if status != http.StatusOK {
		t.Fatalf("client_credentials grant: status %d: %+v", status, resp)
	}
	m2mToken := resp.field(t, "access_token")

	// ---- M2M token reaches its own tenant's protected resource ----
	m2m := newClient(srv.URL)
	m2m.access = m2mToken
	status, resp = m2m.do(t, http.MethodGet, "/api/v1/m2m/tenant/summary", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("m2m tenant summary: status %d: %+v", status, resp)
	}
	if got := resp.field(t, "tenant_id"); got != tenantA {
		t.Fatalf("CROSS-TENANT LEAK (m2m token): got tenant_id=%s, want %s", got, tenantA)
	}

	// ---- Wrong client_secret is rejected ----
	status, _ = newClient(srv.URL).do(t, http.MethodPost, "/oauth/token", nil, map[string]string{
		"grant_type": "client_credentials", "client_id": clientID, "client_secret": "wrong-secret",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("client_credentials grant with wrong secret: status=%d, want 401", status)
	}
}

// accessFromCookie reads the browser-style access token the API just set in
// the client's cookie jar. Browser sessions never put a token in a body.
func (c *client) accessFromCookie(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(c.base + "/api/v1/")
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	if v := cookieValue(c.http.Jar.Cookies(u), "racetify_access"); v != "" {
		return v
	}
	t.Fatalf("no racetify_access cookie in the jar")
	return ""
}

func cookieValue(cookies []*http.Cookie, name string) string {
	for _, ck := range cookies {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}
