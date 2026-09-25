//go:build integration

// endpoints_test.go is the companion to http_test.go's TestFullPhase0Flow
// and storage_test.go's TestObjectStoragePutGetFlow: those two already
// exercise the "happy path" end to end (register -> tenant -> invite ->
// M2M grant -> object storage), so this file's job is breadth rather than
// depth - one test function per resource area, covering every remaining
// HTTP endpoint (everything except Object Storage, which storage_test.go
// already owns) and its important error paths: validation failures, RBAC
// denials, rate limiting, and not-found/conflict cases.
//
// See http_test.go for the shared newTestServer/newClient/client.do/
// registerAndLogin infrastructure this file builds on.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/platform/database"
)

// ---- shared helpers specific to this file ----

// syncBuffer is an io.Writer safe for concurrent use by the slog JSON
// handler (the app logs from multiple goroutines) and this test's reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// extractVerificationToken scans captured JSON log lines for the
// "mailer: verification email" message addressed to wantEmail and returns
// the token embedded in its "link" field's query string - see
// newTestServerWithLogger's doc comment for why this is the only way to
// obtain one in a test.
func extractVerificationToken(t *testing.T, logs, wantEmail string) string {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Msg  string `json:"msg"`
			To   string `json:"to"`
			Link string `json:"link"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Msg != "mailer: verification email" || entry.To != wantEmail {
			continue
		}
		i := strings.Index(entry.Link, "token=")
		if i < 0 {
			t.Fatalf("verification link has no token= query param: %s", entry.Link)
		}
		return entry.Link[i+len("token="):]
	}
	t.Fatalf("no verification email logged for %s (logs: %s)", wantEmail, logs)
	return ""
}

// rawJSONDo issues a request bypassing client.do's cookie jar, which
// silently refuses to reattach a Secure cookie (setRefreshCookie in
// auth_handler.go) over a plain http:// httptest.Server URL - needed to
// test /api/v1/auth/refresh and /api/v1/auth/logout, which read the
// racetify_refresh_token cookie. Mirrors storage_test.go's rawDo, but
// speaks the {"data":...}/{"error":...} JSON envelope and accepts extra
// headers/cookies instead of a raw content-type.
func rawJSONDo(t *testing.T, method, url string, headers map[string]string, cookies []*http.Cookie, body any) (int, apiResponse, *http.Response) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var parsed apiResponse
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed, resp
}

// makeSuperAdmin flips is_super_admin directly in Postgres - there is no
// API path to do this (by design: it's a platform-operator action). Uses
// the admin-role DSN via a direct database.Open, the same
// direct-DB-access pattern internal/repository/rls_isolation_test.go
// establishes for integration tests that need to reach around the
// service layer.
func makeSuperAdmin(t *testing.T, userID string) {
	t.Helper()
	cfg := config.DBConfig{
		DSN:             mustEnv(t, "DATABASE_ADMIN_URL"),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE users SET is_super_admin = true WHERE id = $1`, userID); err != nil {
		t.Fatalf("flip is_super_admin: %v", err)
	}
}

// ---- health ----

func TestHealthEndpoints(t *testing.T) {
	srv := newTestServer(t)
	c := newClient(srv.URL)

	status, resp := c.do(t, http.MethodGet, "/healthz", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /healthz: status %d: %+v", status, resp)
	}
	if got := resp.field(t, "status"); got != "ok" {
		t.Fatalf("GET /healthz: status field=%q, want ok", got)
	}

	status, resp = c.do(t, http.MethodGet, "/readyz", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /readyz: status %d: %+v", status, resp)
	}
	var ready map[string]string
	if err := json.Unmarshal(resp.Data, &ready); err != nil {
		t.Fatalf("decode /readyz data: %v (raw: %s)", err, string(resp.Data))
	}
	if ready["postgres"] != "ok" || ready["redis"] != "ok" {
		t.Fatalf("GET /readyz: got %+v, want both postgres and redis ok", ready)
	}
}

// ---- auth: register/login validation ----

func TestAuthRegisterValidation(t *testing.T) {
	srv := newTestServer(t)
	c := newClient(srv.URL)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "register-validation-" + unique + "@example.com"

	cases := []struct {
		name string
		body map[string]string
	}{
		{"missing email", map[string]string{"password": "supersecret1", "first_name": "A"}},
		{"short password", map[string]string{"email": email, "password": "short", "first_name": "A"}},
		{"missing first_name", map[string]string{"email": email, "password": "supersecret1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := c.do(t, http.MethodPost, "/api/v1/auth/register", nil, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("register with %s: status %d, want 400: %+v", tc.name, status, resp)
			}
			if resp.Error == nil || resp.Error.Code != "invalid_request" {
				t.Fatalf("register with %s: error=%+v, want code invalid_request", tc.name, resp.Error)
			}
		})
	}

	// Duplicate email -> 409 already_exists.
	status, resp := c.do(t, http.MethodPost, "/api/v1/auth/register", nil, map[string]string{
		"email": email, "password": "supersecret1", "first_name": "A",
	})
	if status != http.StatusCreated {
		t.Fatalf("initial register: status %d: %+v", status, resp)
	}
	status, resp = c.do(t, http.MethodPost, "/api/v1/auth/register", nil, map[string]string{
		"email": email, "password": "supersecret1", "first_name": "A",
	})
	if status != http.StatusConflict {
		t.Fatalf("duplicate register: status %d, want 409: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "already_exists" {
		t.Fatalf("duplicate register: error=%+v, want code already_exists", resp.Error)
	}
}

func TestAuthLoginAndSession(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "login-" + unique + "@example.com"

	registerClient := newClient(srv.URL)
	registerAndLogin(t, registerClient, email, "Login User")

	// Correct login.
	c := newClient(srv.URL)
	status, resp := c.do(t, http.MethodPost, "/api/v1/auth/login", nil, map[string]string{
		"email": email, "password": "supersecret1",
	})
	if status != http.StatusOK {
		t.Fatalf("login: status %d: %+v", status, resp)
	}
	if c.accessFromCookie(t) == "" {
		t.Fatalf("login: empty access_token")
	}
	var session struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(resp.Data, &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if session.User.Email != email {
		t.Fatalf("login: user.email=%q, want %q", session.User.Email, email)
	}

	// Wrong password.
	status, resp = newClient(srv.URL).do(t, http.MethodPost, "/api/v1/auth/login", nil, map[string]string{
		"email": email, "password": "wrong-password",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("login wrong password: status %d, want 401: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_credentials" {
		t.Fatalf("login wrong password: error=%+v, want code invalid_credentials", resp.Error)
	}

	// Unknown email.
	status, resp = newClient(srv.URL).do(t, http.MethodPost, "/api/v1/auth/login", nil, map[string]string{
		"email": "nobody-" + unique + "@example.com", "password": "supersecret1",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("login unknown email: status %d, want 401: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_credentials" {
		t.Fatalf("login unknown email: error=%+v, want code invalid_credentials", resp.Error)
	}
}

func TestAuthLoginRateLimit(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	c := newClient(srv.URL) // one client -> one synthetic IP -> one shared rate-limit bucket.

	// router.go: m.loginRateLimit is 10/min keyed by client IP. Fire 11
	// rapid attempts (wrong password is fine - RateLimit runs before the
	// handler even looks at credentials) and expect the 11th to be
	// throttled.
	var lastStatus int
	var lastResp apiResponse
	for i := 0; i < 11; i++ {
		lastStatus, lastResp = c.do(t, http.MethodPost, "/api/v1/auth/login", nil, map[string]string{
			"email": "rate-limit-" + unique + "@example.com", "password": "wrong-password",
		})
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("11th login attempt: status %d, want 429: %+v", lastStatus, lastResp)
	}
	if lastResp.Error == nil || lastResp.Error.Code != "rate_limited" {
		t.Fatalf("11th login attempt: error=%+v, want code rate_limited", lastResp.Error)
	}
}

func TestAuthMe(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "me-" + unique + "@example.com"

	c := newClient(srv.URL)
	registerAndLogin(t, c, email, "Me User")

	status, resp := c.do(t, http.MethodGet, "/api/v1/users/me", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/users/me: status %d: %+v", status, resp)
	}
	if got := resp.field(t, "email"); got != email {
		t.Fatalf("GET /api/v1/users/me: email=%q, want %q", got, email)
	}

	// Unauthenticated.
	anon := newClient(srv.URL)
	status, resp = anon.do(t, http.MethodGet, "/api/v1/users/me", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/users/me unauthenticated: status %d, want 401: %+v", status, resp)
	}
}

func TestAuthRefreshAndLogout(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "refresh-" + unique + "@example.com"

	reg := newClient(srv.URL)
	status, resp := reg.do(t, http.MethodPost, "/api/v1/auth/register", nil, map[string]string{
		"email": email, "password": "supersecret1", "first_name": "Refresh",
	})
	if status != http.StatusCreated {
		t.Fatalf("register: status %d: %+v", status, resp)
	}

	// Manual login via rawJSONDo to capture the raw Set-Cookie header -
	// client.do never exposes response headers, and even if it did, the
	// jar itself won't resend a Secure cookie over this http:// URL (see
	// rawJSONDo's doc comment).
	loginStatus, loginResp, loginHTTPResp := rawJSONDo(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"X-Forwarded-For": reg.ip}, nil, map[string]string{"email": email, "password": "supersecret1"})
	if loginStatus != http.StatusOK {
		t.Fatalf("login: status %d: %+v", loginStatus, loginResp)
	}
	firstAccessToken := cookieValue(loginHTTPResp.Cookies(), "racetify_access")
	cookies := loginHTTPResp.Cookies()
	if len(cookies) == 0 {
		t.Fatalf("login response set no cookies (expected %s)", "racetify_refresh_token")
	}

	// Refresh rotates the token and returns a new session.
	refreshStatus, refreshResp, refreshHTTPResp := rawJSONDo(t, http.MethodPost, srv.URL+"/api/v1/auth/refresh",
		map[string]string{"X-Forwarded-For": reg.ip, "Origin": "http://localhost:3000"}, cookies, nil)
	if refreshStatus != http.StatusOK {
		t.Fatalf("refresh: status %d: %+v", refreshStatus, refreshResp)
	}
	newAccessToken := cookieValue(refreshHTTPResp.Cookies(), "racetify_access")
	if newAccessToken == firstAccessToken {
		t.Fatalf("refresh returned the same access token as login")
	}
	newCookies := refreshHTTPResp.Cookies()
	if len(newCookies) == 0 {
		t.Fatalf("refresh response set no rotated cookie")
	}

	// Reusing the original (now-rotated-away) refresh cookie is theft-
	// response territory: RefreshAccessToken revokes every session for the
	// user and returns invalid_credentials.
	reuseStatus, reuseResp, _ := rawJSONDo(t, http.MethodPost, srv.URL+"/api/v1/auth/refresh",
		map[string]string{"X-Forwarded-For": reg.ip, "Origin": "http://localhost:3000"}, cookies, nil)
	if reuseStatus != http.StatusUnauthorized {
		t.Fatalf("reusing rotated-away refresh cookie: status %d, want 401: %+v", reuseStatus, reuseResp)
	}
	if reuseResp.Error == nil || reuseResp.Error.Code != "invalid_credentials" {
		t.Fatalf("reusing rotated-away refresh cookie: error=%+v, want code invalid_credentials", reuseResp.Error)
	}

	// Logout (with the latest access token + latest refresh cookie)
	// revokes the refresh token and blacklists the access token's jti.
	logoutHeaders := map[string]string{"Authorization": "Bearer " + newAccessToken, "X-Forwarded-For": reg.ip}
	logoutStatus, logoutResp, _ := rawJSONDo(t, http.MethodPost, srv.URL+"/api/v1/auth/logout", logoutHeaders, newCookies, nil)
	if logoutStatus != http.StatusOK {
		t.Fatalf("logout: status %d: %+v", logoutStatus, logoutResp)
	}

	// The just-logged-out access token must now be rejected.
	postLogout := newClient(srv.URL)
	postLogout.access = newAccessToken
	status, resp = postLogout.do(t, http.MethodGet, "/api/v1/users/me", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/users/me after logout: status %d, want 401: %+v", status, resp)
	}
}

func TestAuthVerifyEmail(t *testing.T) {
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, nil))
	srv := newTestServerWithLogger(t, log)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "verify-" + unique + "@example.com"

	c := newClient(srv.URL)
	status, resp := c.do(t, http.MethodPost, "/api/v1/auth/register", nil, map[string]string{
		"email": email, "password": "supersecret1", "first_name": "Verify",
	})
	if status != http.StatusCreated {
		t.Fatalf("register: status %d: %+v", status, resp)
	}

	token := extractVerificationToken(t, logBuf.String(), email)

	// Garbage token -> 404 not_found.
	status, resp = c.do(t, http.MethodPost, "/api/v1/auth/verify-email", nil, map[string]string{"token": "not-a-real-token"})
	if status != http.StatusNotFound {
		t.Fatalf("verify-email garbage token: status %d, want 404: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "not_found" {
		t.Fatalf("verify-email garbage token: error=%+v, want code not_found", resp.Error)
	}

	// Real token -> success.
	status, resp = c.do(t, http.MethodPost, "/api/v1/auth/verify-email", nil, map[string]string{"token": token})
	if status != http.StatusOK {
		t.Fatalf("verify-email: status %d: %+v", status, resp)
	}

	// Re-using the now-consumed token -> 400 token_consumed.
	status, resp = c.do(t, http.MethodPost, "/api/v1/auth/verify-email", nil, map[string]string{"token": token})
	if status != http.StatusBadRequest {
		t.Fatalf("verify-email re-use: status %d, want 400: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "token_consumed" {
		t.Fatalf("verify-email re-use: error=%+v, want code token_consumed", resp.Error)
	}
}

func TestGoogleOAuthNotConfigured(t *testing.T) {
	srv := newTestServer(t)
	c := newClient(srv.URL)

	// newTestServer never sets GOOGLE_CLIENT_ID/SECRET/REDIRECT_URL, so
	// GoogleOAuthService.Enabled() is false.
	status, resp := c.do(t, http.MethodGet, "/api/v1/auth/google/login", nil, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("GET google/login: status %d, want 501: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "not_configured" {
		t.Fatalf("GET google/login: error=%+v, want code not_configured", resp.Error)
	}

	// Missing code/state.
	status, resp = c.do(t, http.MethodGet, "/api/v1/auth/google/callback", nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("GET google/callback missing params: status %d, want 400: %+v", status, resp)
	}

	// code/state present but never issued via a real BeginAuthorization
	// call, so the CSRF state lookup in Redis misses -> invalid_credentials.
	status, resp = c.do(t, http.MethodGet, "/api/v1/auth/google/callback?code=fake&state=fake", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("GET google/callback fabricated params: status %d, want 401: %+v", status, resp)
	}
}

// ---- tenant ----

func TestTenantCreateAndListMine(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())

	c := newClient(srv.URL)
	registerAndLogin(t, c, "tenant-create-"+unique+"@example.com", "Tenant Owner")

	// Unauthenticated.
	status, resp := newClient(srv.URL).do(t, http.MethodPost, "/api/v1/tenants", nil, map[string]string{"name": "x"})
	if status != http.StatusUnauthorized {
		t.Fatalf("create tenant unauthenticated: status %d, want 401: %+v", status, resp)
	}

	// Empty name.
	status, resp = c.do(t, http.MethodPost, "/api/v1/tenants", nil, map[string]string{"name": "  "})
	if status != http.StatusBadRequest {
		t.Fatalf("create tenant empty name: status %d, want 400: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_request" {
		t.Fatalf("create tenant empty name: error=%+v, want code invalid_request", resp.Error)
	}

	// Success.
	name := "List Mine Tenant " + unique
	status, resp = c.do(t, http.MethodPost, "/api/v1/tenants", nil, map[string]string{"name": name})
	if status != http.StatusCreated {
		t.Fatalf("create tenant: status %d: %+v", status, resp)
	}
	tenantID := resp.field(t, "id")
	if got := resp.field(t, "status"); got != "pending_verification" {
		t.Fatalf("create tenant: status field=%q, want pending_verification", got)
	}

	// ListMine reflects it with role owner.
	status, resp = c.do(t, http.MethodGet, "/api/v1/tenants/me", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list tenants/me: status %d: %+v", status, resp)
	}
	var mine []struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(resp.Data, &mine); err != nil {
		t.Fatalf("decode tenants/me: %v (raw: %s)", err, string(resp.Data))
	}
	found := false
	for _, tw := range mine {
		if tw.ID == tenantID {
			found = true
			if tw.Role != "owner" {
				t.Fatalf("tenants/me: role=%q for created tenant, want owner", tw.Role)
			}
		}
	}
	if !found {
		t.Fatalf("tenants/me did not include the just-created tenant %s: %+v", tenantID, mine)
	}
}

// setupTenantWithOwner registers+logs in a fresh Owner, creates a tenant,
// and switches the owner's access token into it (see client.switchTenant -
// the owner is a member of their own just-created tenant by construction,
// so this can never fail), returning the now tenant-scoped owner client
// and the new tenant's id - the starting point most of the RBAC-flavored
// tests below need. A caller that specifically wants a tenant-less token
// (there is none among today's callers) would switch back out by simply
// not using this helper.
func setupTenantWithOwner(t *testing.T, srv string, unique string) (*client, string) {
	t.Helper()
	owner := newClient(srv)
	registerAndLogin(t, owner, "rbac-owner-"+unique+"@example.com", "RBAC Owner")
	status, resp := owner.do(t, http.MethodPost, "/api/v1/tenants", nil, map[string]string{"name": "RBAC Tenant " + unique})
	if status != http.StatusCreated {
		t.Fatalf("create tenant: status %d: %+v", status, resp)
	}
	tenantID := resp.field(t, "id")

	status, _ = owner.switchTenant(t, tenantID)
	if status != http.StatusOK {
		t.Fatalf("owner switching into own just-created tenant: status %d", status)
	}
	return owner, tenantID
}

func TestTenantMembersAndInvitationsRBAC(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, tenantID := setupTenantWithOwner(t, srv.URL, unique)

	// Owner invites an Admin (only an Owner may grant Admin).
	adminEmail := "rbac-admin-" + unique + "@example.com"
	status, resp := owner.do(t, http.MethodPost, "/api/v1/invitations", nil,
		map[string]string{"email": adminEmail, "role": "admin"})
	if status != http.StatusCreated {
		t.Fatalf("owner invite admin: status %d: %+v", status, resp)
	}
	adminToken := resp.field(t, "token")

	admin := newClient(srv.URL)
	registerAndLogin(t, admin, adminEmail, "RBAC Admin")
	status, resp = admin.do(t, http.MethodPost, "/api/v1/invitations/accept", nil, map[string]string{"token": adminToken})
	if status != http.StatusOK {
		t.Fatalf("admin accept invitation: status %d: %+v", status, resp)
	}
	status, _ = admin.switchTenant(t, tenantID)
	if status != http.StatusOK {
		t.Fatalf("admin switching into tenant after accepting invitation: status %d", status)
	}

	// Admin inviting as Admin -> 403 forbidden (only Owner may grant Admin).
	status, resp = admin.do(t, http.MethodPost, "/api/v1/invitations", nil,
		map[string]string{"email": "someone-else-" + unique + "@example.com", "role": "admin"})
	if status != http.StatusForbidden {
		t.Fatalf("admin inviting as admin: status %d, want 403: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "forbidden" {
		t.Fatalf("admin inviting as admin: error=%+v, want code forbidden", resp.Error)
	}

	// Admin inviting as Staff -> succeeds (Admin may invite Staff).
	staffEmail := "rbac-staff-" + unique + "@example.com"
	status, resp = admin.do(t, http.MethodPost, "/api/v1/invitations", nil,
		map[string]string{"email": staffEmail, "role": "staff"})
	if status != http.StatusCreated {
		t.Fatalf("admin invite staff: status %d: %+v", status, resp)
	}
	staffToken := resp.field(t, "token")
	staffInvitationID := resp.field(t, "id")

	// ListMembers: owner + admin so far.
	status, resp = owner.do(t, http.MethodGet, "/api/v1/members", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list members: status %d: %+v", status, resp)
	}
	var memberPage struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(resp.Data, &memberPage); err != nil {
		t.Fatalf("decode members page: %v", err)
	}
	if len(memberPage.Items) != 2 {
		t.Fatalf("list members: got %d items, want 2 (owner+admin)", len(memberPage.Items))
	}

	// ListInvitations.
	status, resp = owner.do(t, http.MethodGet, "/api/v1/invitations", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list invitations: status %d: %+v", status, resp)
	}

	// AcceptInvitation with wrong email (a different user account accepts
	// staffEmail's invitation) -> 403 forbidden.
	wrongUser := newClient(srv.URL)
	registerAndLogin(t, wrongUser, "rbac-wrong-"+unique+"@example.com", "Wrong User")
	status, resp = wrongUser.do(t, http.MethodPost, "/api/v1/invitations/accept", nil, map[string]string{"token": staffToken})
	if status != http.StatusForbidden {
		t.Fatalf("accept invitation wrong email: status %d, want 403: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "forbidden" {
		t.Fatalf("accept invitation wrong email: error=%+v, want code forbidden", resp.Error)
	}

	// Invalid token -> 404 not_found.
	status, resp = wrongUser.do(t, http.MethodPost, "/api/v1/invitations/accept", nil, map[string]string{"token": "garbage"})
	if status != http.StatusNotFound {
		t.Fatalf("accept invitation invalid token: status %d, want 404: %+v", status, resp)
	}

	// Real staff member accepts.
	staff := newClient(srv.URL)
	registerAndLogin(t, staff, staffEmail, "RBAC Staff")
	status, resp = staff.do(t, http.MethodPost, "/api/v1/invitations/accept", nil, map[string]string{"token": staffToken})
	if status != http.StatusOK {
		t.Fatalf("staff accept invitation: status %d: %+v", status, resp)
	}

	// Already-accepted (non-pending) -> 400 invalid_request.
	status, resp = staff.do(t, http.MethodPost, "/api/v1/invitations/accept", nil, map[string]string{"token": staffToken})
	if status != http.StatusBadRequest {
		t.Fatalf("re-accept already-accepted invitation: status %d, want 400: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_request" {
		t.Fatalf("re-accept already-accepted invitation: error=%+v, want code invalid_request", resp.Error)
	}

	// RevokeInvitation: revoke the (already-accepted, but revocation just
	// flips status - the repository doesn't care) staff invitation once,
	// then again -> 404 not_found since Revoke's UPDATE ... WHERE id=...
	// no longer matches a row it can transition (see
	// InvitationRepository.Revoke / checkRowsAffected).
	status, resp = owner.do(t, http.MethodDelete, "/api/v1/invitations/"+staffInvitationID, nil, nil)
	firstRevokeStatus := status
	firstRevokeResp := resp
	status, resp = owner.do(t, http.MethodDelete, "/api/v1/invitations/"+staffInvitationID, nil, nil)
	if status != http.StatusNotFound {
		// The invitation was already accepted (not pending) before this
		// first revoke call too, so both calls may 404 - either way, a
		// revoke on an invitation that can't be transitioned again must
		// 404, which is exactly the assertion this test cares about.
		t.Fatalf("second revoke of same invitation: status %d, want 404 (first revoke was status %d: %+v)", status, firstRevokeStatus, firstRevokeResp)
	}

	// Revoke of a nonexistent invitation id -> 404 not_found.
	status, resp = owner.do(t, http.MethodDelete, "/api/v1/invitations/00000000-0000-0000-0000-000000000000", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("revoke nonexistent invitation: status %d, want 404: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "not_found" {
		t.Fatalf("revoke nonexistent invitation: error=%+v, want code not_found", resp.Error)
	}
}

func TestTenantSuperAdminSetStatus(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())

	// A regular (non-super-admin) member is forbidden.
	nonAdmin, tenantID := setupTenantWithOwner(t, srv.URL, unique)
	status, resp := nonAdmin.do(t, http.MethodPatch, "/api/v1/admin/tenants/"+tenantID+"/status", nil,
		map[string]any{"status": "active"})
	if status != http.StatusForbidden {
		t.Fatalf("non-super-admin set status: status %d, want 403: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "forbidden" {
		t.Fatalf("non-super-admin set status: error=%+v, want code forbidden", resp.Error)
	}

	// Make a fresh user a super admin, then log in again so the freshly
	// issued access token actually carries is_super_admin=true (a token
	// issued before the DB flip would not reflect it - see
	// middleware.RequireUserAuth, which reads the JWT claim, not the DB).
	admin := newClient(srv.URL)
	adminEmail := "super-admin-" + unique + "@example.com"
	registerAndLogin(t, admin, adminEmail, "Super Admin")
	status, resp = admin.do(t, http.MethodGet, "/api/v1/users/me", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("get admin id: status %d: %+v", status, resp)
	}
	adminUserID := resp.field(t, "id")
	makeSuperAdmin(t, adminUserID)

	status, resp = admin.do(t, http.MethodPost, "/api/v1/auth/login", nil, map[string]string{
		"email": adminEmail, "password": "supersecret1",
	})
	if status != http.StatusOK {
		t.Fatalf("re-login after super-admin flip: status %d: %+v", status, resp)
	}
	admin.access = admin.accessFromCookie(t)

	// Invalid status value.
	status, resp = admin.do(t, http.MethodPatch, "/api/v1/admin/tenants/"+tenantID+"/status", nil,
		map[string]any{"status": "not-a-real-status"})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid status value: status %d, want 400: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_request" {
		t.Fatalf("invalid status value: error=%+v, want code invalid_request", resp.Error)
	}

	// Rejecting requires a reason.
	status, resp = admin.do(t, http.MethodPatch, "/api/v1/admin/tenants/"+tenantID+"/status", nil,
		map[string]any{"status": "rejected"})
	if status != http.StatusBadRequest {
		t.Fatalf("reject without reason: status %d, want 400: %+v", status, resp)
	}

	// Reject with a reason -> success.
	status, resp = admin.do(t, http.MethodPatch, "/api/v1/admin/tenants/"+tenantID+"/status", nil,
		map[string]any{"status": "rejected", "reason": "incomplete documents"})
	if status != http.StatusOK {
		t.Fatalf("reject with reason: status %d: %+v", status, resp)
	}
	if got := resp.field(t, "status"); got != "rejected" {
		t.Fatalf("reject with reason: status field=%q, want rejected", got)
	}

	// Suspending requires a reason too.
	status, resp = admin.do(t, http.MethodPatch, "/api/v1/admin/tenants/"+tenantID+"/status", nil,
		map[string]any{"status": "suspended"})
	if status != http.StatusBadRequest {
		t.Fatalf("suspend without reason: status %d, want 400: %+v", status, resp)
	}
	status, resp = admin.do(t, http.MethodPatch, "/api/v1/admin/tenants/"+tenantID+"/status", nil,
		map[string]any{"status": "suspended", "reason": "fraud report"})
	if status != http.StatusOK {
		t.Fatalf("suspend with reason: status %d: %+v", status, resp)
	}

	// Reactivating to active never needs a reason and clears any prior one.
	status, resp = admin.do(t, http.MethodPatch, "/api/v1/admin/tenants/"+tenantID+"/status", nil,
		map[string]any{"status": "active"})
	if status != http.StatusOK {
		t.Fatalf("reactivate: status %d: %+v", status, resp)
	}
	if got := resp.field(t, "status"); got != "active" {
		t.Fatalf("reactivate: status field=%q, want active", got)
	}

	// Nonexistent tenant id -> 404 not_found.
	status, resp = admin.do(t, http.MethodPatch, "/api/v1/admin/tenants/00000000-0000-0000-0000-000000000000/status", nil,
		map[string]any{"status": "active"})
	if status != http.StatusNotFound {
		t.Fatalf("set status on nonexistent tenant: status %d, want 404: %+v", status, resp)
	}
}

// ---- OAuth clients + client_credentials grant ----

func TestOAuthClientsCRUDAndTokenGrant(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, _ := setupTenantWithOwner(t, srv.URL, unique)

	// Invalid scope.
	status, resp := owner.do(t, http.MethodPost, "/api/v1/oauth-clients", nil,
		map[string]any{"name": "Bad Scope Client", "scopes": []string{"not:a:real:scope"}})
	if status != http.StatusBadRequest {
		t.Fatalf("create client invalid scope: status %d, want 400: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_scope" {
		t.Fatalf("create client invalid scope: error=%+v, want code invalid_scope", resp.Error)
	}

	// Empty name.
	status, resp = owner.do(t, http.MethodPost, "/api/v1/oauth-clients", nil,
		map[string]any{"name": "  ", "scopes": []string{"tenant:read"}})
	if status != http.StatusBadRequest {
		t.Fatalf("create client empty name: status %d, want 400: %+v", status, resp)
	}

	// Success.
	status, resp = owner.do(t, http.MethodPost, "/api/v1/oauth-clients", nil,
		map[string]any{"name": "Timing System", "scopes": []string{"tenant:read", "events:read"}})
	if status != http.StatusCreated {
		t.Fatalf("create client: status %d: %+v", status, resp)
	}
	rowID := resp.field(t, "id")
	clientID := resp.field(t, "client_id")
	clientSecret := resp.field(t, "client_secret")

	// List.
	status, resp = owner.do(t, http.MethodGet, "/api/v1/oauth-clients", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list clients: status %d: %+v", status, resp)
	}
	var listPage struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(resp.Data, &listPage); err != nil {
		t.Fatalf("decode client list: %v", err)
	}
	if len(listPage.Items) != 1 {
		t.Fatalf("list clients: got %d items, want 1", len(listPage.Items))
	}

	// Unsupported grant_type.
	status, resp = newClient(srv.URL).do(t, http.MethodPost, "/oauth/token", nil, map[string]string{
		"grant_type": "password", "client_id": clientID, "client_secret": clientSecret,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("token unsupported grant_type: status %d, want 400: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "unsupported_grant_type" {
		t.Fatalf("token unsupported grant_type: error=%+v, want code unsupported_grant_type", resp.Error)
	}

	// Missing client_id/client_secret.
	status, resp = newClient(srv.URL).do(t, http.MethodPost, "/oauth/token", nil, map[string]string{
		"grant_type": "client_credentials",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("token missing credentials: status %d, want 400: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_request" {
		t.Fatalf("token missing credentials: error=%+v, want code invalid_request", resp.Error)
	}

	// Successful grant.
	status, resp = newClient(srv.URL).do(t, http.MethodPost, "/oauth/token", nil, map[string]string{
		"grant_type": "client_credentials", "client_id": clientID, "client_secret": clientSecret,
	})
	if status != http.StatusOK {
		t.Fatalf("token grant: status %d: %+v", status, resp)
	}
	if resp.field(t, "access_token") == "" {
		t.Fatalf("token grant: empty access_token")
	}

	// Revoke.
	status, resp = owner.do(t, http.MethodDelete, "/api/v1/oauth-clients/"+rowID, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("revoke client: status %d: %+v", status, resp)
	}

	// Revoke again: unlike InvitationRepository.Revoke (which filters WHERE
	// status = 'pending', so a second revoke 404s), internal/oauthclient's
	// Repository.Revoke's UPDATE has no status filter - it matches on
	// id+tenant_id alone, so re-revoking an already-revoked client is
	// idempotent and still returns 200, not 404.
	status, resp = owner.do(t, http.MethodDelete, "/api/v1/oauth-clients/"+rowID, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("revoke client again (idempotent): status %d, want 200: %+v", status, resp)
	}

	// Revoke of a nonexistent client id -> 404 not_found.
	status, resp = owner.do(t, http.MethodDelete, "/api/v1/oauth-clients/00000000-0000-0000-0000-000000000000", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("revoke nonexistent client: status %d, want 404: %+v", status, resp)
	}

	// A revoked client can no longer complete the grant.
	status, resp = newClient(srv.URL).do(t, http.MethodPost, "/oauth/token", nil, map[string]string{
		"grant_type": "client_credentials", "client_id": clientID, "client_secret": clientSecret,
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("token grant with revoked client: status %d, want 401: %+v", status, resp)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_credentials" {
		t.Fatalf("token grant with revoked client: error=%+v, want code invalid_credentials", resp.Error)
	}
}
