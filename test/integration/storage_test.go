//go:build integration

// TestObjectStoragePutGetFlow is the HTTP-level companion to
// internal/platform/objectstorage/objectstorage_test.go and
// internal/storage/service.go's doc comments: it drives the real
// router through httptest to prove the whole presign -> PUT -> presign ->
// GET round trip works end to end (not just the signing math in
// isolation), that a private object is unreachable without a valid
// signature, and that RequestDownload's tenant-scoped lookup - the primary
// cross-tenant defense described on storage.Service.RequestDownload -
// actually stops a different tenant's Owner from resolving another
// tenant's object, over real HTTP.
package integration

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// rawDo issues a request with an arbitrary method/body/content-type,
// bypassing client.do's JSON envelope assumptions - needed for the object
// PUT/GET endpoints, which speak raw bytes, not the {"data":...} envelope.
func rawDo(t *testing.T, method, url, contentType string, body []byte) (int, []byte, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, got, resp.Header
}

func TestObjectStoragePutGetFlow(t *testing.T) {
	srv := newTestServer(t)
	unique := fmt.Sprintf("%d", time.Now().UnixNano())

	ownerA := newClient(srv.URL)
	ownerB := newClient(srv.URL)
	registerAndLogin(t, ownerA, "storage-owner-a-"+unique+"@example.com", "Owner A")
	registerAndLogin(t, ownerB, "storage-owner-b-"+unique+"@example.com", "Owner B")

	status, resp := ownerA.do(t, http.MethodPost, "/api/v1/tenants", nil, map[string]string{"name": "Storage Tenant A " + unique})
	if status != http.StatusCreated {
		t.Fatalf("create tenant A: status %d: %+v", status, resp)
	}
	tenantA := resp.field(t, "id")

	status, resp = ownerB.do(t, http.MethodPost, "/api/v1/tenants", nil, map[string]string{"name": "Storage Tenant B " + unique})
	if status != http.StatusCreated {
		t.Fatalf("create tenant B: status %d: %+v", status, resp)
	}
	tenantB := resp.field(t, "id")

	// Every ownerA.do call below used to carry an X-Tenant-ID: tenantA
	// header per request; now tenant selection happens once, at
	// token-issuance time (see internal/httpapi/middleware/tenant.go's
	// RequireTenantForUser doc comment), so a single switch up front
	// covers every subsequent tenant-scoped call this client makes.
	status, _ = ownerA.switchTenant(t, tenantA)
	if status != http.StatusOK {
		t.Fatalf("owner A switching into own tenant: status %d", status)
	}

	// ---- private bucket: presign upload ----
	key := "bib-templates/" + unique + ".txt"
	status, resp = ownerA.do(t, http.MethodPost, "/api/v1/storage/objects/upload-url", nil,
		map[string]string{"bucket": "private", "key": key, "content_type": "text/plain"})
	if status != http.StatusOK {
		t.Fatalf("request upload url: status %d: %+v", status, resp)
	}
	uploadURL := resp.field(t, "upload_url")

	// ---- PUT the actual bytes at the presigned (unauthenticated) URL ----
	payload := []byte("BIB template contents " + unique)
	putStatus, putBody, _ := rawDo(t, http.MethodPut, srv.URL+uploadURL, "text/plain", payload)
	if putStatus != http.StatusOK {
		t.Fatalf("PUT object: status %d: %s", putStatus, string(putBody))
	}

	// ---- A tampered signature must be rejected before touching disk ----
	tamperedURL := srv.URL + uploadURL + "TAMPERED"
	tamperStatus, _, _ := rawDo(t, http.MethodPut, tamperedURL, "text/plain", []byte("attacker bytes"))
	if tamperStatus != http.StatusForbidden {
		t.Fatalf("PUT with tampered signature: status=%d, want 403", tamperStatus)
	}

	// ---- presign download and fetch it back ----
	status, resp = ownerA.do(t, http.MethodGet, "/api/v1/storage/objects/download-url?bucket=private&key="+key, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("request download url: status %d: %+v", status, resp)
	}
	downloadURL := resp.field(t, "url")

	getStatus, getBody, getHeaders := rawDo(t, http.MethodGet, srv.URL+downloadURL, "", nil)
	if getStatus != http.StatusOK {
		t.Fatalf("GET object: status %d: %s", getStatus, string(getBody))
	}
	if !bytes.Equal(getBody, payload) {
		t.Fatalf("GET object: got %q, want %q", getBody, payload)
	}
	if ct := getHeaders.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("GET object: Content-Type=%q, want text/plain", ct)
	}

	// ---- List surfaces the stored object ----
	status, resp = ownerA.do(t, http.MethodGet, "/api/v1/storage/objects", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list objects: status %d: %+v", status, resp)
	}

	// ---- CROSS-TENANT: owner B cannot even provision a download URL for
	// tenant A's private object - RequestDownload's tenant-scoped GetByKey
	// lookup (the primary defense per storage.Service's doc comment) must
	// fail closed with not_found before any signature is ever minted. Owner
	// B *can* select their own tenant (they're its owner) - the isolation
	// lives in the resource lookup below, not in tenant selection itself,
	// unlike http_test.go's ownerA-into-tenantB probe.
	status, _ = ownerB.switchTenant(t, tenantB)
	if status != http.StatusOK {
		t.Fatalf("owner B switching into own tenant: status %d", status)
	}
	status, _ = ownerB.do(t, http.MethodGet, "/api/v1/storage/objects/download-url?bucket=private&key="+key, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("CROSS-TENANT LEAK: tenant B resolved a download URL for tenant A's private object, status=%d, want 404", status)
	}

	// ---- CROSS-TENANT, defense in depth: even if tenant B forged a
	// download URL shaped like tenant A's (same key, tenant B's own
	// tenant_id in the path), the signature is scoped to tenant A's id and
	// must not verify. ----
	forgedURL := srv.URL + "/api/v1/storage/objects/private/" + tenantB + "/" + key + "?expires=9999999999&sig=deadbeef"
	forgedStatus, _, _ := rawDo(t, http.MethodGet, forgedURL, "", nil)
	if forgedStatus != http.StatusForbidden {
		t.Fatalf("forged cross-tenant object URL: status=%d, want 403", forgedStatus)
	}

	// ---- public bucket: PublicURL needs no signature at all ----
	publicKey := "logos/" + unique + ".txt"
	status, resp = ownerA.do(t, http.MethodPost, "/api/v1/storage/objects/upload-url", nil,
		map[string]string{"bucket": "public", "key": publicKey, "content_type": "text/plain"})
	if status != http.StatusOK {
		t.Fatalf("request public upload url: status %d: %+v", status, resp)
	}
	publicUploadURL := resp.field(t, "upload_url")

	publicPayload := []byte("tenant logo bytes " + unique)
	putStatus, putBody, _ = rawDo(t, http.MethodPut, srv.URL+publicUploadURL, "text/plain", publicPayload)
	if putStatus != http.StatusOK {
		t.Fatalf("PUT public object: status %d: %s", putStatus, string(putBody))
	}

	publicURL := srv.URL + "/api/v1/storage/objects/public/" + tenantA + "/" + publicKey
	pubStatus, pubBody, _ := rawDo(t, http.MethodGet, publicURL, "", nil)
	if pubStatus != http.StatusOK {
		t.Fatalf("GET public object (unsigned): status %d: %s", pubStatus, string(pubBody))
	}
	if !bytes.Equal(pubBody, publicPayload) {
		t.Fatalf("GET public object: got %q, want %q", pubBody, publicPayload)
	}

	// ---- RBAC: Staff cannot provision an upload URL (admin+ only) ----
	staffEmail := "storage-staff-" + unique + "@example.com"
	status, resp = ownerA.do(t, http.MethodPost, "/api/v1/invitations", nil,
		map[string]string{"email": staffEmail, "role": "staff"})
	if status != http.StatusCreated {
		t.Fatalf("invite staff: status %d: %+v", status, resp)
	}
	rawToken := resp.field(t, "token")

	staff := newClient(srv.URL)
	registerAndLogin(t, staff, staffEmail, "Storage Staff")
	status, resp = staff.do(t, http.MethodPost, "/api/v1/invitations/accept", nil, map[string]string{"token": rawToken})
	if status != http.StatusOK {
		t.Fatalf("accept invitation: status %d: %+v", status, resp)
	}
	status, _ = staff.switchTenant(t, tenantA)
	if status != http.StatusOK {
		t.Fatalf("staff switching into tenant A after accepting invitation: status %d", status)
	}

	status, _ = staff.do(t, http.MethodPost, "/api/v1/storage/objects/upload-url", nil,
		map[string]string{"bucket": "private", "key": "staff-attempt/" + unique + ".txt", "content_type": "text/plain"})
	if status != http.StatusForbidden {
		t.Fatalf("staff requesting upload url: status=%d, want 403 (staff is below admin)", status)
	}
}
