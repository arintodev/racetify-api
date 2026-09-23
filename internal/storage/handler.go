package storage

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// maxUploadBytes bounds a single object PUT body. Sized for Phase 0's
// actual files (tenant logos, BIB/certificate templates, individual event
// photos) - see objectstorage.go's package doc for why Put/Get hold a whole
// object in memory. Phase 1's bulk CSV import or high-resolution photo
// uploads should get their own explicit, larger limit once that feature
// exists, rather than this ceiling being silently raised for everyone.
const maxUploadBytes = 25 << 20 // 25 MiB

// Handler is split into two halves that intentionally sit behind
// different parts of the middleware chain (see this package's
// routes.go):
//   - the control-plane endpoints (RequestUpload/RequestDownload/List)
//     require a normal tenant-scoped user session, like every other
//     bounded context;
//   - the data-plane endpoints (PutObject/GetObject) are deliberately
//     unauthenticated at the HTTP layer - the presigned URL's signature IS
//     the authorization, exactly like internal/tenant's Invitation or
//     internal/oauthclient's token-possession endpoints (see Repository's
//     type doc comment).
type Handler struct {
	storage *Service
	store   objectstorage.Driver
}

func NewHandler(storage *Service, store objectstorage.Driver) *Handler {
	return &Handler{storage: storage, store: store}
}

// ---- control-plane: tenant-session authenticated ----

type requestUploadRequest struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
}

type completeUploadRequest struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// CompleteUpload handles POST /api/v1/storage/objects/complete-upload.
// Required for STORAGE_DRIVER=r2 only, after the client has PUT bytes
// directly to R2 using the presigned URL RequestUpload issued - see
// Service.CompleteUpload's doc comment for why "local" needs no
// equivalent call. Gated the same as RequestUpload (RoleAdmin+).
func (h *Handler) CompleteUpload(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())

	var req completeUploadRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}

	size, err := h.storage.CompleteUpload(r.Context(), tenantID, actorUserID, rbac.MemberRole(actorRoleStr), req.Bucket, req.Key)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]any{"stored": true, "size_bytes": size})
}

// RequestUpload handles POST /api/v1/storage/objects/upload-url.
// Requires RequireTenantForUser + RequireRole(RoleAdmin) - see
// Service.RequestUpload's doc comment for why upload provisioning is
// gated higher than read/list.
func (h *Handler) RequestUpload(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	actorUserID, _ := reqctx.UserID(r.Context())
	actorRoleStr, _ := reqctx.MemberRole(r.Context())

	var req requestUploadRequest
	if !respond.DecodeJSON(w, r, &req) {
		return
	}

	ticket, err := h.storage.RequestUpload(r.Context(), tenantID, actorUserID, rbac.MemberRole(actorRoleStr), req.Bucket, req.Key, req.ContentType)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, UploadTicketDTO{
		UploadURL: ticket.UploadURL, ExpiresAt: ticket.ExpiresAt, Bucket: ticket.Bucket, Key: ticket.Key,
	})
}

// RequestDownload handles GET /api/v1/storage/objects/download-url?bucket=&key=.
// Any active tenant member may call this (read-only, provisions nothing) -
// see Service.RequestDownload's doc comment for the tenant-isolation
// guarantee it relies on.
func (h *Handler) RequestDownload(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")

	ticket, err := h.storage.RequestDownload(r.Context(), tenantID, key, bucket)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	dto := DownloadTicketDTO{URL: ticket.URL}
	if !ticket.ExpiresAt.IsZero() {
		dto.ExpiresAt = &ticket.ExpiresAt
	}
	respond.JSON(w, http.StatusOK, dto)
}

// List handles GET /api/v1/storage/objects. Paginated:
// ?limit=&cursor=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := reqctx.TenantID(r.Context())
	page, err := h.storage.ListObjects(r.Context(), tenantID, respond.PageParamsFromRequest(r))
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	out := make([]ObjectDTO, 0, len(page.Items))
	for i := range page.Items {
		out = append(out, objectResponse(&page.Items[i]))
	}
	respond.JSON(w, http.StatusOK, respond.ListResponseDTO[ObjectDTO]{Items: out, NextCursor: page.NextCursor})
}

// ---- data-plane: unauthenticated, signature-verified ----

// PutObject handles PUT {objectstorage.ObjectURLPath}/{bucket}/{tenantID}/{key...}?expires=&sig=.
// Mounted outside every auth middleware in router.go by design. Only
// applies to a ProxyDriver ("local") - see proxyDriverOrNotImplemented's
// doc comment for what happens when the active driver is "r2" instead (its
// presigned URLs point straight at R2, so no client should ever reach this
// endpoint for it, but a stray or malicious request still gets a clean
// error rather than a panic).
func (h *Handler) PutObject(w http.ResponseWriter, r *http.Request) {
	proxy, ok := h.proxyDriverOrNotImplemented(w)
	if !ok {
		return
	}
	bucket, tenantID, key, ok := h.verifyPresignedRequest(w, r, proxy, http.MethodPut)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	data, err := objectstorage.ReadAll(r.Body)
	if err != nil {
		respond.Error(w, http.StatusRequestEntityTooLarge, "payload_too_large", "Upload exceeds the maximum allowed size.")
		return
	}

	sha256Hex, size, err := proxy.Put(bucket, tenantID, key, data)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := h.storage.CompletePut(r.Context(), tenantID, bucket, key, sha256Hex, size); err != nil {
		respond.FromServiceError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]any{"stored": true, "sha256": sha256Hex, "size_bytes": size})
}

// GetObject handles GET {objectstorage.ObjectURLPath}/{bucket}/{tenantID}/{key...}[?expires=&sig=].
// The public bucket needs no signature at all (see verifyPresignedRequest);
// the private bucket does. Only applies to a ProxyDriver ("local") - see
// PutObject's doc comment.
func (h *Handler) GetObject(w http.ResponseWriter, r *http.Request) {
	proxy, ok := h.proxyDriverOrNotImplemented(w)
	if !ok {
		return
	}
	bucket, tenantID, key, ok := h.verifyPresignedRequest(w, r, proxy, http.MethodGet)
	if !ok {
		return
	}

	obj, err := h.storage.ResolveForGet(r.Context(), tenantID, bucket, key)
	if err != nil {
		respond.FromServiceError(w, err)
		return
	}
	data, err := proxy.Get(bucket, tenantID, key)
	if err != nil {
		respond.Error(w, http.StatusNotFound, "not_found", "The requested object was not found.")
		return
	}
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// proxyDriverOrNotImplemented type-asserts h.store to objectstorage.
// ProxyDriver, writing a clean 501 and returning ok=false when the active
// driver (STORAGE_DRIVER=r2) doesn't satisfy it - see objectstorage.go's
// package doc for why only "local" is a ProxyDriver.
func (h *Handler) proxyDriverOrNotImplemented(w http.ResponseWriter) (objectstorage.ProxyDriver, bool) {
	proxy, ok := h.store.(objectstorage.ProxyDriver)
	if !ok {
		respond.Error(w, http.StatusNotImplemented, "not_implemented", "The active storage driver does not proxy object bytes through this API - use the presigned URL directly.")
		return nil, false
	}
	return proxy, true
}

// verifyPresignedRequest parses the {bucket}/{tenantID}/{key...} path
// segments objectstorage.Store.presign encodes into every ticket URL, and
// - except for a public-bucket GET, which PublicURL deliberately issues
// with no signature at all (it is meant to be world-readable, per the
// guide's bucket split) - verifies the ?expires=&sig= signature before
// letting the caller touch anything on disk.
func (h *Handler) verifyPresignedRequest(w http.ResponseWriter, r *http.Request, proxy objectstorage.ProxyDriver, method string) (bucket objectstorage.Bucket, tenantID, key string, ok bool) {
	bucket = objectstorage.Bucket(r.PathValue("bucket"))
	tenantID = r.PathValue("tenantID")
	key = r.PathValue("key")
	if !bucket.Valid() || tenantID == "" || key == "" {
		respond.Error(w, http.StatusBadRequest, "invalid_request", "bucket, tenant id, and key are required.")
		return "", "", "", false
	}

	if method == http.MethodGet && bucket == objectstorage.BucketPublic {
		return bucket, tenantID, key, true
	}

	expiresUnix, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
	sig := r.URL.Query().Get("sig")
	if err != nil || sig == "" {
		respond.Error(w, http.StatusForbidden, "forbidden", "A valid presigned URL is required.")
		return "", "", "", false
	}
	if err := proxy.VerifySignature(method, bucket, tenantID, key, expiresUnix, sig); err != nil {
		status := http.StatusForbidden
		if errors.Is(err, objectstorage.ErrURLExpired) {
			status = http.StatusGone
		}
		respond.Error(w, status, "forbidden", "This presigned URL is invalid or has expired.")
		return "", "", "", false
	}
	return bucket, tenantID, key, true
}
