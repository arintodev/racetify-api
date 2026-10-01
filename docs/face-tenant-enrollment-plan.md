# Racetify API — Tenant Face Enrollment via OAuth2 Client Credentials (Planning)

> Planning document, written before any code for this feature exists.
> Extends `internal/face` (docs/face-search-plan.md, racetify-app repo) and
> `internal/oauthclient` (docs/phase0-refactor-plan.md's M2M grant) rather
> than replacing either. Nothing in this document is implemented yet.

## 1. Problem

`internal/face` today lets a Racetify user enroll their own face for one
event and search that event's gallery by it (`POST
/api/v1/events/{id}/users/{uid}/face-embeddings`). Both `faces` and
`face_embeddings` hard-require `event_id` and `user_id` (FK to `users`),
i.e. an enrolled face is always "this Racetify account's face, for this
one event."

A tenant now wants to offer the same face-search feature to *their own*
end users through a server-to-server integration (OAuth 2.0 Client
Credentials, `internal/oauthclient`) — but those end users:

* are **not** Racetify accounts and never will be (the tenant's backend
  never calls the Participant API to register them either — this
  integration is *only* the face-search feature, nothing else),
* are identified by whatever opaque identifier the tenant's own system
  already uses for them (`ref_id`),
* are not tied to one event — a tenant's own user should be enrollable
  once and searchable against any of that tenant's event galleries
  without re-enrolling per event.

This requires `faces`/`face_embeddings` to support a second kind of
enrolled subject alongside the existing Racetify-user one, with different
scoping rules for each.

## 2. Model

### 2.1 Two kinds of enrolled subject, one table

A `faces` row's subject is **exactly one** of:

1. **`user_id`** — a Racetify account, self-enrolled. Global to that
   user, not tied to any tenant or event: the same face is usable to
   search *any* event's gallery the account can otherwise reach (staff
   role, or an event assignment/capability). This matches the existing
   Single Identity principle already used elsewhere (one account, usable
   across events).
2. **`ref_id`** — an opaque string the *tenant* assigns to their own end
   user. Meaningful only within that tenant (`tenant_id` is required),
   never tied to a specific event either — enroll once, search across
   any of that tenant's events.

Neither kind of row is event-scoped. Event scoping for face search comes
entirely from the *gallery* side (`photo_face_detections.photo_id →
photos.event_id`), which is unaffected by this change — a detected face
in a photo has always belonged to whichever event that photo belongs to.
`faces`/`face_embeddings` themselves drop `event_id` entirely.

```
                     ┌───────────────┐
   Racetify user ───▶│  faces.user_id │── global (tenant_id NULL)
                     └───────────────┘        │
                                               ▼ searchable against
   Tenant's own   ┌───────────────┐      any event's gallery the
   end user      ─▶│  faces.ref_id  │──    caller has access to
   (via M2M)       └───────────────┘   (tenant_id required, scoped
                                         to that one tenant's rows)
```

### 2.2 Schema (`faces`, `face_embeddings`)

```sql
-- both tables get the same shape of change:
ALTER TABLE faces DROP COLUMN event_id;              -- and its FK/index
ALTER TABLE faces ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE faces ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE faces ADD COLUMN ref_id TEXT NULL;

ALTER TABLE faces ADD CONSTRAINT faces_subject_xor CHECK (
    (user_id IS NOT NULL AND ref_id IS NULL AND tenant_id IS NULL)
    OR
    (ref_id IS NOT NULL AND user_id IS NULL AND tenant_id IS NOT NULL)
);
ALTER TABLE faces ADD CONSTRAINT faces_ref_id_format CHECK (
    ref_id IS NULL OR ref_id ~ '^[A-Za-z0-9_.:-]{1,200}$'
);

-- one face per Racetify user, globally (no tenant/event in the key)
CREATE UNIQUE INDEX faces_user_uk ON faces (user_id) WHERE user_id IS NOT NULL;
-- one face per (tenant, ref_id) - not per event
CREATE UNIQUE INDEX faces_tenant_ref_uk ON faces (tenant_id, ref_id) WHERE ref_id IS NOT NULL;
```

`face_embeddings` gets the identical treatment (drop `event_id`, nullable
`user_id`/`tenant_id`, new `ref_id`, same CHECK pair) so an enrollment
record's subject is always unambiguous and matches its parent `faces`
row.

`faces.consent_revoked_at` and the `Revoked()`/`consent_revoked` error
path are dropped along with the old soft `RevokeConsent` operation (see
§3.2) — withdrawing consent now means deleting the row (`DropFace`), so
there is nothing left to flag as revoked-but-present. `consented_at`
stays: the enroll request still requires an explicit consent flag before
any row is created at all.

`photo_face_detections` is untouched — still `photo_id → photos.event_id`,
still no face_id, per the existing "detected faces are never clustered or
assigned" invariant (`internal/face/face.go`'s package doc comment).

### 2.3 Row-Level Security

The straightforward RLS shape (`tenant_id = current_setting(...)`,
`internal/httpapi`'s usual pattern) can't work here: a global user-owned
row (`tenant_id IS NULL`) still needs to be **readable from inside any
tenant's transaction**, because a tenant's own staff must be able to
search a Racetify user's face against that tenant's event gallery. This
is deliberately looser than the `audit_logs` pattern (migrations/0003),
where a NULL-tenant row is only visible with *no* tenant context at all —
here it must be visible *regardless* of tenant context:

```sql
CREATE POLICY tenant_isolation ON faces
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
```

Consequence: any query that only wants "my tenant's own `ref_id` rows"
(list, drop) **must filter `ref_id IS NOT NULL` explicitly in SQL** — RLS
alone will also let a global row through. Search deliberately relies on
this looseness (see §3.3); list/drop deliberately guard against it
(see §3.2).

Self-enroll (`user_id` subject) runs through `db.WithTx` (no tenant
context set at all — there is no tenant in that call path to begin with),
so `current_setting('app.tenant_id', true)` is unset and only the
`tenant_id IS NULL` branch of the policy ever matches, which is exactly
the row it's allowed to touch.

## 3. Service (`internal/face`)

### 3.1 Enroll

Two entrypoints, deliberately not unified, because their transaction
scope and audit shape differ:

```go
// Self-enroll: no tenant at all. Runs in db.WithTx.
func (s *Service) Enroll(ctx context.Context, userID, actorUserID string,
    imageBytes []byte, filename string, consent bool) (*Face, error)

// Tenant M2M enroll: runs in db.WithTenantTx(tenantID, ...).
func (s *Service) EnrollByRef(ctx context.Context, tenantID, refID, actorClientID string,
    imageBytes []byte, filename string, consent bool, consentReference string) (*Face, error)
```

Both keep the existing "first enrollment creates the `faces` row,
subsequent ones add a `face_embeddings` row under it" behavior from
today's `Enroll` — just keyed by `user_id` or `(tenant_id, ref_id)`
instead of `(tenant_id, event_id, user_id)`.

`EnrollByRef`'s response includes `face_id` — the tenant's backend is
responsible for persisting it on their side; there is no lookup-by-ref_id
search path (see §3.3), so losing it means losing the ability to search
by that enrollment (re-enrolling under the same `ref_id` recovers the
same `face_id` via the unique index, so it's not unrecoverable, just
requires an extra image).

### 3.2 List / Drop

`ListEmbeddings` / `ListEmbeddingsByRef` mirror the two enroll paths, same
transaction scoping.

There is no separate "revoke consent" operation (the old `RevokeConsent`,
which soft-deleted embeddings but kept a `faces` tombstone, is removed
along with `consent_revoked_at`/`Revoked()`). Withdrawing consent is the
same action as deleting the enrollment outright — one method, hard
delete, used by both paths:

```go
// DropFace hard-deletes a face: the faces row, all its face_embeddings,
// and their Qdrant points. Frees the subject (user_id or ref_id) for
// re-enrollment. Used by both paths; ownerCheck pins it to the caller's
// own subject on each.
func (s *Service) DropFace(ctx context.Context, faceID string, ownerCheck func(*Face) bool) error
```

* self path: `ownerCheck` requires `tenant_id IS NULL AND user_id == actorUserID`
* M2M path: `ownerCheck` requires `ref_id IS NOT NULL` (RLS already
  confines the row to the caller's own tenant; this stops an M2M client
  from dropping a Racetify user's global face, which RLS's loosened
  policy would otherwise let through)

Recorded as audit action `face.dropped`.

### 3.3 Search — unchanged shape, no ownership check

This is intentionally the *simplest* piece, matching the original
pre-existing `Search` code: given a `face_id`, if the RLS-scoped lookup
finds it, search proceeds — no `user_id`/`ref_id` ownership comparison at
all.

```go
func (s *Service) Search(ctx context.Context, tenantID, eventID, faceID string,
    page pagination.PageParams) (pagination.Page[gallery.Photo], error)
```

Both the user-session route and the M2M route call this same method, with
`tenantID`/`eventID` resolved the same way routes already resolve them
today (`RequireEventAccess` for staff/crew, the M2M token's tenant for
the client route). Because of §2.3's RLS, `faceID` can point at:

* a global Racetify-user face (any account, any tenant) — always visible, or
* a `ref_id` face belonging to this exact tenant — visible only here.

This is the concrete effect of "an admin/staff caller can search with any
`face_id`": there's no per-caller restriction to add or maintain, it falls
out of the RLS shape directly. A `face_id` is an unguessable UUID, handed
out only to whoever the enrollment happened for (or the tenant that
performed it) — the same trust model the original implementation already
used.

### 3.4 Qdrant payload

`qdrantstore.Payload` gets a `RefID` field alongside the existing
`UserID` (metadata only, `omitempty`, never filtered on at search time —
only `TenantID`/`EventID`/`Source` are, and those come from the
*detection* side, unaffected by any of this).

## 4. OAuth2 scopes (`internal/oauthclient/scope.go`)

```go
ScopeFaceWrite = "face:write" // enroll, drop
ScopeFaceRead  = "face:read"  // list, search
```

Provisioned the same way every other M2M scope is — `POST
/api/v1/oauth-clients` by a tenant Admin/Owner (`internal/oauthclient`,
unchanged).

## 5. Endpoints (`internal/face/routes.go`)

```
POST   /api/v1/users/{uid}/face-enrollment              self-enroll, user-session, no tenant
GET    /api/v1/users/{uid}/face-enrollment               {uid} must equal the caller's own id
DELETE /api/v1/faces/{fid}                              user-session drop (hard delete, frees account for re-enrollment); ownership via user_id (§3.2)

POST   /api/v1/clients/face-enrollment                   M2M enroll; body {ref_id, image, consent, consent_reference}
GET    /api/v1/clients/face-enrollment?ref_id=...         M2M list embeddings by ref_id
DELETE /api/v1/clients/faces/{fid}                       M2M drop (hard delete, frees ref_id for reuse); ref_id IS NOT NULL + tenant RLS (§3.2)

POST   /api/v1/events/{id}/faces/search                  user-session, body {face_id} — staff/crew via RequireEventAccess
POST   /api/v1/clients/events/{id}/faces/search          M2M, body {face_id}
```

`ref_id` deliberately never appears in a URL path (tenant's own user
identifiers, potentially sensitive) — only in a JSON body (enroll) or a
query parameter (list).

Middleware chains follow existing patterns exactly:

* self-enroll/list/drop: `mw.RequireUserAuth` only (no tenant
  middleware at all — there is no tenant in this path).
* M2M enroll/list/drop: `middleware.RequireScope(oauthclient.ScopeFaceWrite|ScopeFaceRead) → middleware.RequireTenantForM2M → mw.RequireM2MAuth`
  (identical shape to `GET /api/v1/m2m/tenant/summary` in `internal/tenant/routes.go`).
* search (both): existing `EventGate.RequireEventAccess(CapabilityFaceSearch)`
  for the user-session route; the M2M-scoped equivalent
  (`RequireScope` + `RequireTenantForM2M` + `RequireM2MAuth`) resolving
  tenant from the M2M token rather than an event assignment.

Distinguishing which path a request came in on needs no new mechanism —
`reqctx.UserID(ctx)` is only ever set by `RequireUserAuth`,
`reqctx.M2MClientID(ctx)` only by `RequireM2MAuth`
(`internal/httpapi/reqctx/reqctx.go`) — and since each path is already a
separate route registration with its own middleware chain, each gets its
own small handler function reading the context key it expects, rather
than one handler branching at runtime.

## 6. Consent

Racetify has no direct relationship with a tenant's end user and cannot
itself verify their consent. The M2M enroll request still requires
`consent: true` (refused with `consent_required` otherwise, same as
today), but for this path it is an **attestation by the tenant**, not a
verification by Racetify. An optional `consent_reference` field lets the
tenant record a pointer to their own consent record; it is stored in the
`face.enrolled` audit log metadata alongside `ref_id` and
`actor_client_id`, so there is a trail if a dispute ever needs one.
Obtaining consent from their own end user is the tenant's contractual
responsibility, not something enforced by this API beyond the flag.

## 7. Compatibility

The self-enroll route path changes (`face-embeddings` → `face-enrollment`,
`event_id` dropped from the path) and the underlying schema changes
(`event_id` dropped, `user_id`/`tenant_id` made nullable) are breaking for
any existing caller of the current endpoints. Before implementing, check
`racetify-app` for any use of:

* `POST/GET /api/v1/events/{id}/users/{uid}/face-embeddings`
* `DELETE /api/v1/events/{id}/faces/{fid}/consent` (replaced by the hard-delete `DropFace`, §3.2)
* `POST /api/v1/events/{id}/faces/search`

and update those call sites alongside the API change.

## 8. Migration checklist

1. New migration (`00NN_face_tenant_enrollment.up/down.sql`): schema
   changes from §2.2, RLS policy replacement from §2.3, for both `faces`
   and `face_embeddings`.
2. `internal/face/face.go`: `Face`/`Embedding` structs drop `EventID`,
   `UserID`/`TenantID` become `*string`, add `RefID *string`.
3. `internal/face/repository.go`: new/renamed queries per §3.1–§3.2
   (`GetFaceByUser` → no event_id; new `GetFaceByRef`,
   `ListEmbeddingsByRef`; `RevokeConsent` replaced by `DropFace`, explicit
   ownership filters on it).
4. `internal/face/service.go`: split `Enroll`/`EnrollByRef`,
   `ListEmbeddings`/`ListEmbeddingsByRef`, `Search` simplified (no
   ownership check, just drops the `event_id` comparison that no longer
   applies).
5. `internal/oauthclient/scope.go`: `ScopeFaceWrite`, `ScopeFaceRead`.
6. `internal/face/routes.go`: endpoints from §5, new M2M handler
   functions alongside the existing user-session ones.
7. `internal/platform/qdrantstore`: `Payload.RefID`.
8. Update `racetify-app` call sites per §7.
9. `docs/openapi.yaml`: reflect the new/changed paths.

## 9. Face-detector service contract (implemented, post-planning addendum)

Once implementation started, the actual `face-detector` service (a separate
repo, `d:\Arintodev\Racetify\face-detector`) turned out to be **URL-based**,
not upload-based: `POST {baseURL}/detect/single`, body `{photo_id, url}`
(`app/schemas.py`'s `BatchPhotoItem`), answering `{status: "success"|
"error", error, faces: [{bbox: [x1,y1,x2,y2] pixels, det_score, embedding,
...}]}` (`PhotoResult`/`FaceResult`). The service fetches the image itself
(`app/image_fetcher.py`) rather than accepting a multipart upload, and
`bbox` is InsightFace's raw pixel `[x1,y1,x2,y2]`, not the fraction-of-
image `Box` this codebase stores everywhere else.

This changed `internal/face/embedclient.go` from the original "detect-
embed, multipart body" sketch to a JSON POST, and required a
`boxFraction(bbox, width, height) Box` conversion in `detectPhoto`
(`detection_job.go`) using the photo's already-known stored dimensions
(`gallery.Photo.Width/Height`) - `landmarks`/`age`/`gender` are part of the
service's response too but unused here.

**Enrollment's temp-upload problem.** Because the service is URL-based,
`Enroll`/`EnrollByRef` need a downloadable URL for the raw enrollment image
bytes they receive over HTTP - there is no raw-bytes endpoint on
face-detector to fall back to. This is in tension with §6's original "the
image itself is never persisted anywhere" framing: it is now written to a
short-lived object, immediately deleted again once the detect call
returns (success or failure), rather than never touching storage at all.

Getting that object a home surfaced a real gap in `internal/storage`: its
`objects` table required `tenant_id NOT NULL`, but a self-enrolled face
(§2.1: global, no tenant) has nothing to scope a tracked object under. This
was fixed the same way `faces.tenant_id` was made nullable
(migrations/0022): `migrations/0023_object_storage_personal.up.sql` makes
both `objects.tenant_id` **and** `objects.created_by` nullable, and RLS now
recognizes two independent scopes on the same table - tenant-scoped (as
before) or **personal**, `tenant_id IS NULL AND created_by = <owning
user>`. A new `db.WithUserTx` (mirroring `WithTenantTx`, setting
`app.user_id` instead of `app.tenant_id`) drives the personal side.

Concretely:

* `Service.tempUploadOwn` (self-enroll): `storage.Service.
  StoreGeneratedPersonal`/`RequestDownloadPersonal`/`DeletePersonal` - a
  personal object, `created_by = userID`, `tenant_id` NULL.
* `Service.tempUploadByRef` (tenant M2M): `storage.Service.
  StoreGenerated`/`RequestDownload`/`DeleteGenerated` (the last one newly
  added) - a tenant-scoped object as usual, but `created_by` is left empty
  (→ SQL NULL) since the actor is an OAuth client, not a `users.id`.
* The unauthenticated data-plane `GetObject`/`PutObject` HTTP endpoints
  (`internal/storage/handler.go`) needed no changes at all:
  `Repository.GetByKeyUnscoped` now matches `tenant_id = $1 OR (tenant_id
  IS NULL AND created_by = $1)` in one query, since the "tenantID" the
  signed URL carries is really just "whichever subject id this object is
  namespaced under" - a tenant's or a user's, disambiguated purely by which
  half of that OR clause has a row.
* Both temp-upload paths only work when `STORAGE_DRIVER=local` (a
  `ProxyDriver`) - the same pre-existing limitation every other
  server-side-generated file in this codebase already has (thumbnails,
  certificates, ...; `StoreGenerated`'s own doc comment). `STORAGE_DRIVER=
  r2` was never asked to grow a new server-side write path for this.
