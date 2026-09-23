package storage

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
)

// RegisterRoutes wires the Object Storage module
// (internal/platform/objectstorage) in two halves:
//
//   - the control plane (provision an upload/download ticket, list what a
//     tenant has stored) sits behind the same tenant-session middleware as
//     every other resource - upload provisioning at RoleAdmin+ (matching
//     internal/tenant's InviteStaff/internal/oauthclient's CreateClient
//     gating), read/list at any active member, matching ListMembers/
//     ListInvitations;
//   - the data plane (the actual PUT/GET of object bytes, at
//     objectstorage.ObjectURLPath) is deliberately mounted with NO auth
//     middleware at all - the presigned URL's own signature is the
//     authorization, verified inside Handler itself. See Repository's type
//     doc comment for why this mirrors the invitation-token/OAuth-client-
//     secret "possession of a secret" pattern already used elsewhere in
//     this codebase.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service, store objectstorage.Driver) {
	h := NewHandler(svc, store)

	// ---- control plane: user-session authenticated, tenant-scoped ----
	mux.Handle("POST /api/v1/storage/objects/upload-url", routing.Chain(h.RequestUpload, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	// complete-upload is only meaningful for STORAGE_DRIVER=r2 (see
	// Service.CompleteUpload's doc comment) but is gated identically to
	// upload-url (RoleAdmin+) regardless of driver, since completing an
	// upload is part of the same provisioning action.
	mux.Handle("POST /api/v1/storage/objects/complete-upload", routing.Chain(h.CompleteUpload, mw.RequireAdminRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/storage/objects/download-url", routing.Chain(h.RequestDownload, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))
	mux.Handle("GET /api/v1/storage/objects", routing.Chain(h.List, mw.RequireAnyRole, mw.RequireTenantForUser, mw.RequireUserAuth))

	// ---- data plane: unauthenticated, signature-verified ----
	objectPattern := objectstorage.ObjectURLPath + "/{bucket}/{tenantID}/{key...}"
	mux.Handle("PUT "+objectPattern, routing.Chain(h.PutObject))
	mux.Handle("GET "+objectPattern, routing.Chain(h.GetObject))
}
