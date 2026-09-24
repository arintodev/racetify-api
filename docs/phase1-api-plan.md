# Racetify API — Phase 1 Endpoint Plan (Market Penetration)

> Planning document, written before any Phase 1 code, per this repo's own
> "API First" standard (`implementation_guide_phase_0.md` §4). Covers the
> three P0 modules from `implementation_guide_phase_1.md` §4 — Data
> Ingestion (CSV), Generator (BIB/E-Cert), Media Gallery & OCR — plus the
> Runner Portal that ties them together. Nothing here is implemented yet;
> this is the contract to build against, laid out the way
> `docs/openapi.yaml` will formalize it once each module lands.
>
> **Status note, added after `docs/phase0-refactor-plan.md` shipped:**
> §9 below proposed adopting a package-per-bounded-context shape *for
> Phase 1* while leaving Phase 0's `internal/domain`/`repository`/
> `service` layering untouched. That premise no longer holds - Phase 0
> itself has since been restructured the same way (`internal/auth`,
> `internal/tenant`, `internal/oauthclient`, `internal/storage`, each
> owning its entity/repository/service/handler/routes together; see
> `docs/phase0-refactor-plan.md`). §9's "Problem 1" (routing) and
> "Problem 2" (composition root) are therefore already solved
> infrastructure, not open design questions: `internal/httpapi/routing`
> (`Middlewares`/`Chain`) exists today, every Phase 0 bounded context
> already self-registers via its own `RegisterRoutes(mux, mw, ...)`, and
> `internal/app.Build()` is already the single composition root. Phase 1
> modules should simply follow that established precedent rather than
> build it. Every other reference below to `internal/service.*`,
> `internal/repository.*`, or `internal/domain`'s now-relocated entities
> (`User`, `Tenant`, `OAuthClient`, `Object`, ...) is similarly stale -
> read those as the bounded-context package they moved to
> (`internal/auth`, `internal/tenant`, `internal/oauthclient`,
> `internal/storage`) rather than literally.

## 1. Scope recap

Phase 0 shipped tenants, users, RBAC, M2M OAuth2, and Object Storage.
Phase 1 adds the business domain the PRD's *Wedge* strategy needs:
Organizers import participant data from a third-party registration/timing
system (CSV) rather than registering through Racetify, and Racetify's job
is BIB printing, E-Certificates, and a photo-matching Media Gallery. The
Runner Journey is **frictionless**: no login, search by BIB or name.

Two consequences that shape the plan below, both already anticipated by
Phase 0:

- **Async work needs a queue.** CSV import, BIB batch PDF generation, and
  photo thumbnail/watermark/OCR must not block an HTTP request. The
  README's "Looking ahead: Phase 1 async jobs" section already recorded
  the design (Redis list-queue + `cmd/worker`, sharing each bounded
  context's service package with `cmd/api`) — this plan builds that now, since
  Phase 1 is the "real job" the README was waiting for.
- **The Runner Portal is unauthenticated by design.** Every other Phase 0
  route requires a JWT or M2M token plus tenant resolution
  (`RequireTenantForUser`/`RequireTenantForM2M`). The public routes below
  need a **third** tenant-resolution mode — by `event_slug` in the URL,
  read-only, no membership check — analogous to how the Object Storage
  data-plane routes already run with no auth middleware and rely on a
  different guarantee (there: a signed URL; here: an event must be
  `published` before it resolves at all).

## 2. New domain entities & migrations

Following the existing `migrations/000N_*.up.sql`/`.down.sql` pattern —
one file per cohesive group, RLS enabled inline in the same file that
creates the table (as `0004_object_storage.up.sql` already does), plain
`TEXT` status/enum columns enforced in Go only (per `0001_core.up.sql`'s
documented reasoning), `set_updated_at` trigger on every mutable table.

| Migration | Tables | Notes |
|---|---|---|
| `0005_storage_providers.up.sql` | `objects` (altered) | **Phase 0 retrofit**, runs first — see the multi-provider subsection below. `ALTER TABLE objects ADD COLUMN provider TEXT NOT NULL DEFAULT '<templated>'`, backfilling every existing row to whatever `STORAGE_DEFAULT_PROVIDER` names, since that's a fact (every row that exists was written by the one driver Phase 0 ever had active), not a guess. |
| `0006_events_races.up.sql` | `events`, `races` | Both `tenant_id`-scoped + RLS. `events.slug` unique **per tenant**; add a second global-uniqueness check only if/when subdomain routing (`event.racetify.id`) needs it literally 1:1 — for now the public routes key off `(tenant resolved via slug lookup)`, see §5. `events.status`: `draft` \| `published` \| `archived` — the Runner Portal only ever resolves `published`. |
| `0007_participants.up.sql` | `participants` | `tenant_id` + `event_id` + `race_id` FKs, RLS. `UNIQUE (event_id, bib_number)` — this is the DB-level backstop for the guide's "sistem harus bisa mengidentifikasi jika `bib_number` duplikat" requirement; the import job validates before insert and this constraint is what makes the upsert-by-`bib_number` path in §4.2 race-safe. `status`: `registered` \| `finisher` (guide's two values; `dns`/`dnf` are a trivial follow-on, not blocking). Index on `(event_id, bib_number)` and a `pg_trgm`-free simple `lower(bib_name)`/`lower(first_name \|\| ' ' \|\| last_name)` index for the portal's name search (§5). |
| `0008_generator_templates.up.sql` | `generator_templates` | `tenant_id` + `event_id` + nullable `race_id` (race-specific override) + `storage_id` FK → `objects(id)` — see the settled storage-design note below §2 for why the FK is back. `service`: `bib` \| `certificate`. `metadata JSONB` for placeholder-field bookkeeping (which `{{TOKENS}}` the SVG uses). |
| `0009_media_gallery.up.sql` | `albums`, `photos`, `photo_tags` | All `tenant_id`-scoped + RLS. `photos.original_storage_id` FK → `objects(id)` (private bucket, high-res) and `photos.thumbnail_storage_id` FK → `objects(id)` (public bucket, watermarked; nullable until the thumbnail job finishes). `photos.ocr_status`: `pending` \| `processed` \| `failed`. `photo_tags.bib_string`/`bib_number` exactly as the guide specifies, plus `confidence_score` and `source`: `ocr` \| `manual` (so a manual correction is distinguishable from a machine tag in the audit trail and in the "needs review" query). |
| `0010_jobs.up.sql` | `jobs` | Generic async-job table, tenant-scoped + RLS, shared by all three async flows below rather than one table per job type: `id, tenant_id, type, status, payload JSONB, result JSONB, error TEXT, progress_current, progress_total, created_by, created_at, updated_at, started_at, finished_at`. `status`: `queued` \| `processing` \| `completed` \| `completed_with_errors` \| `failed`. `type`: `participants.import` \| `generator.bib_batch` \| `media.photo_process`. Mirrors the README's own prediction: "the `objects` table's `status` column ... are patterns a job-status column ... can mirror directly." |
| `0013_participant_ref_id.up.sql` | `participants.ref_id` | Optional `TEXT` reference ID from the organizer's registration system (order/ticket number), unique per event when present (partial unique index `(event_id, ref_id) WHERE ref_id IS NOT NULL`). The registration import's primary match key when mapped — see §4.2. |

**Storage location design — settled, after two revisions.** Worth
recording how this landed, since it moved twice: draft 1 put
`photos`/`generator_templates` behind an `object_id` FK into `objects`.
Revised to inline `bucket`/`key`/... columns directly on each row, on the
concern that `objects` (and the single active `STORAGE_DRIVER`
underneath it) tied every file to one storage backend, which didn't fit
"platform should hold files across R2/S3/other-S3-compatible/Google
Drive/local at once." That concern is legitimate, but it turned out to be
a property of `internal/platform/objectstorage` (fixed below, as the
multi-provider `Registry`), not of the `objects` table itself — once
`objects` carries a `provider` column (`0005_storage_providers.up.sql`
above), an FK into it is exactly as portable across backends as inline
columns would be, and normalizing "where is this file" into one place
platform-wide is simpler than repeating `provider`/`bucket`/`key`/
`content_type`/`size_bytes`/`sha256` on every table that owns a file. So:
**settled on the FK, renamed `object_id` → `storage_id`** — `photos`
(§0009) and `generator_templates` (§0008) both reference `objects(id)`.
The one real tradeoff worth watching, not blocking: `objects`' `UNIQUE
(tenant_id, bucket, object_key)` + upsert-on-conflict semantics were built
for a *few, occasionally-replaced* file (a tenant logo) — photos are
*thousands, never replaced*, so each upload is simply a fresh `objects`
row (a plain insert, never hitting that unique constraint's conflict
path) rather than a genuine reuse of the upsert behavior. That's a
non-issue correctness-wise; it just means `objects` grows one row per
photo, which is fine at Postgres scale and is what keyset pagination
(`internal/platform/pagination`) already exists for.

This does **not** reintroduce a storage-backend dependency — the thing
that actually makes local/R2/S3/"storage lain" swappable is
`internal/platform/objectstorage`'s `Driver` interface
(`PresignUpload`/`PresignDownload`/`PublicURL`, taking a bare
`(bucket, key)` and knowing nothing about `objects` at all), not the
`objects` table. `gallery/service.go` and `generator/service.go` don't
touch `objectstorage` directly at all now — they depend on
`internal/storage.Service` (Phase 0, unchanged in shape) exactly
the way Phase 0's own handler already does: `RequestUpload` to presign,
`CompleteUpload`/`CompletePut` to confirm, `ResolveForGet` to read back.
A gallery bulk-upload just calls `storage.Service.RequestUpload` once per
photo inside its own batch handler (§4.4) — one HTTP round trip for the
caller, N ordinary Phase 0 calls underneath, nothing new to build there.
The one remaining Phase 1 touchpoint that was *never* in question is the
CSV file in §4.2's `participants/import/preview` — always a plain call to
the general `POST .../storage/objects/upload-url` (Phase 0, unchanged),
since it's a one-shot input consumed once by the import job, not a column
on any lasting row.

**Multi-provider storage (platform-wide, not per-tenant).** The
requirement underneath all of the above: the platform should be able to
hold files across *several* backends at once — R2, AWS S3, another
S3-compatible provider, Google Drive, local, etc. — not a per-tenant
choice, a platform-wide capability to use more than one concurrently.
`internal/platform/objectstorage` as Phase 0 built it can't do this:
`NewDriver(cfg) Driver` constructs exactly **one** active driver
process-wide, picked once by `STORAGE_DRIVER=local|r2`. This is a genuine
Phase 0 module change, and it's what makes the FK decision above safe:
`objects.provider` is the actual source of multi-backend flexibility: any
row referencing it — Phase 0's own tenant-logo/generic uploads, and now
`photos`/`generator_templates` via `storage_id` — gets that flexibility
for free, in one place, instead of it needing to be re-derived per table.

Redesign: `NewDriver` becomes `NewRegistry`, building a named map of
drivers instead of activating one:

```go
type Provider string // a *name* the platform assigns, e.g. "local", "r2primary",
                      // "s3backup", "gdrive" — not a Go type per backend kind

type Registry struct { /* map[Provider]Driver, built once at boot */ }
func (r *Registry) Driver(p Provider) (Driver, error)  // unknown/misconfigured -> clear error, fails closed
func (r *Registry) Default() Provider                   // STORAGE_DEFAULT_PROVIDER
```

The `Driver`/`ProxyDriver`/`DirectDriver` interfaces and both existing
implementations (`local_driver.go`, `r2_driver.go`) are untouched — what
changes is that a *kind* can now be instantiated more than once under
different names with different settings. That already covers most of the
list: R2, AWS S3, and "other S3-compatible" are one kind today
(`r2_driver.go`'s own doc comment: it "works against any other
S3-compatible bucket, including real AWS S3, by pointing
STORAGE_R2_ENDPOINT at a different host") — the registry just lets two
named instances of it (e.g. `r2primary` + `s3backup`) run at once instead
of picking one globally. Google Drive is the one genuinely new *kind*: no
presigned-PUT concept, so it follows `local_driver.go`'s `ProxyDriver`
shape (bytes relayed through this server's own PUT/GET endpoint) rather
than `r2_driver.go`'s `DirectDriver` shape — real, separately-scoped work
(OAuth credential flow, Drive's resumable-upload/`files.get` API), not
part of this planning pass. The registry is what makes adding it later a
new driver file, not another redesign.

`objects` now carries **three** storage columns, not two: `provider`,
`bucket`, `key` (§0005) — and because every Phase 1 file-owning row
reaches storage *through* `objects` via `storage_id` (the design settled
above), that's the only place these three columns need to exist at all.
A row's `provider` is fixed at write time and never silently
reinterpreted: changing `STORAGE_DEFAULT_PROVIDER` only decides where
*new* uploads land — every existing `objects` row keeps resolving through
whichever provider it actually has, for as long as that provider stays
configured in the registry. That's what makes changing (or adding) the
default non-breaking for everything already stored, Phase 0 and Phase 1
alike. `storage.Service` itself needs no behavior change beyond taking a
`*Registry` and always resolving `Registry.Default()` for a fresh upload
— nothing above it (Phase 0's tenant-logo/generic-upload/CSV-import
paths, or `gallery`/`generator` calling the same service) needs
per-request provider choice; only the plumbing type underneath `storage.
Service` changes, from one `Driver` to a `*Registry`.

Config stays flat, named env vars — matches this repo's existing
`.env.example` convention (and the Laravel/AdonisJS "disk" comparison
`objectstorage.go`'s own package doc already draws) better than a JSON
blob in one var, and the provider count here is small and enumerable, not
open-ended:

```
STORAGE_PROVIDERS=local,r2primary          # which provider names are active
STORAGE_DEFAULT_PROVIDER=r2primary         # where new uploads land

STORAGE_PROVIDER_LOCAL_KIND=local
STORAGE_PROVIDER_LOCAL_ROOT_DIR=./data/storage
...                                         # local's existing STORAGE_* vars, renamed under this prefix

STORAGE_PROVIDER_R2PRIMARY_KIND=s3compatible
STORAGE_PROVIDER_R2PRIMARY_ENDPOINT=...
STORAGE_PROVIDER_R2PRIMARY_BUCKET=...
...                                         # today's STORAGE_R2_* vars, renamed under this prefix, repeatable per provider name
```

New `internal/domain` files: **only** what's genuinely shared platform
concepts, following the existing shape (plain struct + string-enum
consts, no framework imports) — `job.go` (the generic job status enum,
since `internal/jobqueue` and every module's job handler all need the
same `JobStatus` type) and nothing else. Every other Phase 1 entity
(`Event`, `Race`, `Participant`, `GeneratorTemplate`, `Album`, `Photo`,
`PhotoTag`) lives inside its own bounded-context package per §9, not in
`internal/domain` — that's the point of the package-per-bounded-context
restructure agreed there: an entity lives with the repository/service
that owns its invariants, not in a shared grab-bag package. Neither
`photos.go` nor `template.go` needs any `domain`/`objectstorage` import
for this — with storage resolved through `storage_id` → `objects`, a
photo/template's own struct only holds the UUID, not a bucket/provider
type; `gallery`/`generator` see buckets and providers only indirectly, by
calling `storage.Service`. New audit actions, appended to the
existing `internal/audit` package's action-constant list (audit logging
stays centralized — it's genuinely cross-cutting):

```
ActionEventCreated, ActionEventStatusChanged, ActionRaceCreated
ActionParticipantImportStarted, ActionParticipantImportCompleted
ActionTemplateUploaded
ActionBibGenerationRequested, ActionCertificateGenerated
ActionPhotoUploaded, ActionPhotoTagCorrected
```

**OAuth scopes:** no new scope constants needed yet. `ScopeEventsRead`/
`ScopeEventsWrite`, `ScopeRegistrationsRead`, and `ScopeTimingWrite`
already exist in `internal/domain/scope.go` and map directly onto the
M2M endpoints below (§4.2's results-push, in particular, is exactly what
`ScopeTimingWrite` was named for in Phase 0).

## 3. Route files

One new `routes_*.go` per resource group, registered in `router.go`
exactly like the six Phase 0 ones — no route collides across files:

```
routes_event.go        events + races (tenant-scoped CRUD)
routes_participant.go  participants CRUD + CSV import + M2M results push
routes_generator.go    templates + BIB batch generation + on-demand cert
routes_media.go        albums + photos + photo_tags
routes_job.go          generic job-status polling (GET only)
routes_public.go       the entire unauthenticated Runner Portal surface
```

## 4. Tenant-scoped & M2M endpoints

Role column reads as the *minimum* `MemberRole` via `RequireTenantForUser`
+ `RequireRole` (`Staff` = any active member, matching the PRD's "Tenant
Staff: mengelola operasional event ... tanpa akses ke keuangan/API keys" —
which is exactly what Phase 1's endpoints are, so most of them sit at
Staff rather than Admin, unlike Storage's current Admin-only upload gate).

### 4.1 Events & Races

| Method & Path | Role | Purpose |
|---|---|---|
| `POST /api/v1/events` | Admin | Create event (draft) |
| `GET /api/v1/events` | Staff | List events, paginated |
| `GET /api/v1/events/{id}` | Staff | Get event |
| `PATCH /api/v1/events/{id}` | Admin | Update event fields |
| `PATCH /api/v1/events/{id}/status` | Admin | `draft→published→archived`; publishing is what makes §5's public routes resolve |
| `POST /api/v1/events/{id}/races` | Admin | Create race/category |
| `GET /api/v1/events/{id}/races` | Staff | List races |
| `PATCH /api/v1/events/{id}/races/{raceId}` | Admin | Update race |
| `DELETE /api/v1/events/{id}/races/{raceId}` | Admin | Remove race (only if no participants reference it) |
| `GET /api/v1/m2m/events` | `events:read` | M2M read, e.g. a tenant's own website listing its events |
| `GET /api/v1/m2m/events/{id}` | `events:read` | M2M read single event |

### 4.2 Participants & Data Ingestion

| Method & Path | Role / Scope | Purpose |
|---|---|---|
| `POST /api/v1/events/{id}/participants` | Staff | Manual single add |
| `GET /api/v1/events/{id}/participants` | Staff | List/search, paginated, filters: `race_id`, `status`, `bib_number`, `q` (name) |
| `GET /api/v1/events/{id}/participants/{pid}` | Staff | Get one |
| `PATCH /api/v1/events/{id}/participants/{pid}` | Staff | Manual edit |
| `DELETE /api/v1/events/{id}/participants/{pid}` | Admin | Remove |
| `GET /api/v1/events/{id}/participants/import/template` | Staff | Download the standard Racetify CSV template (guide's CSV-format risk mitigation) |
| `POST /api/v1/events/{id}/participants/import/preview` | Staff | Body: `object_id` of an already-uploaded CSV (via the existing Storage `upload-url`/`PUT` flow). **Synchronous** — just parses headers + first N rows, returns them for the Column Mapping UI. No DB writes. |
| `POST /api/v1/events/{id}/participants/import` | Staff | Body: `object_id`, `column_mapping`, `mode: registration \| results`. Validates mapping, then enqueues a `participants.import` job and returns `202 {job_id}`. `mode=results` is the post-race upsert-by-`bib_number` path (`net_time`/`gun_time`/`overall_rank`/`category_rank`); `mode=registration` is the pre-race insert/identify-duplicates path. |
| `POST /api/v1/m2m/events/{id}/results` | `timing:write` | Bulk upsert by `bib_number`, same semantics as `mode=results` above but for a timing system that pushes directly instead of exporting CSV — this is the scope Phase 0 already named for exactly this use case. |

**Matching an import row to an existing participant (`mode=registration`).**
A row is matched on `ref_id` first, when the column is mapped and the
cell is non-empty, and on `bib_number` otherwise:

| Match | Outcome |
|---|---|
| `ref_id` matches, same BIB | Existing participant (skip or update, per the import's option) |
| `ref_id` matches, different BIB | Existing participant whose **BIB changed**; reported as such in the preview. The update is rejected if the new BIB already belongs to someone else in the event |
| `ref_id` is new, BIB free | New participant |
| `ref_id` is new, BIB taken by a participant with a different `ref_id` | Error: BIB belongs to another registration |
| `ref_id` is new, BIB taken by a participant with no `ref_id` | Existing participant (matched by BIB); an update fills in its `ref_id` |
| No `ref_id` in the row | Match by `bib_number`, as before |

`mode=results` (and the M2M results push) keep matching on `bib_number`
only, since timing systems only know the BIB.

### 4.3 Generator (Templates, BIB, E-Certificate)

| Method & Path | Role | Purpose |
|---|---|---|
| `POST /api/v1/events/{id}/templates` | Admin | Body: `{storage_id, service, race_id?, name, metadata}`. `storage_id` comes from the *existing* Phase 0 flow — call `POST .../storage/objects/upload-url` (private bucket), PUT the SVG, then register the template pointing at that object. No template-specific upload endpoint needed |
| `GET /api/v1/events/{id}/templates` | Staff | List |
| `PATCH /api/v1/events/{id}/templates/{tid}` | Admin | Body: optionally a new `storage_id` (upload a replacement SVG via the same Phase 0 flow first, then rebind) and/or `metadata` |
| `DELETE /api/v1/events/{id}/templates/{tid}` | Admin | Remove |
| `POST /api/v1/events/{id}/races/{raceId}/bib-generation` | Admin | Body: `template_id`, participant filter. Enqueues `generator.bib_batch`, returns `202 {job_id}` |
| `GET /api/v1/events/{id}/bib-generation/{jobId}/download` | Staff | Presigned download URL for the generated bundle (private bucket), once the job (polled via §4.5) is `completed` |

E-Certificate is deliberately **not** a tenant-scoped endpoint — it is
runner-facing, on-demand, and unauthenticated by design (guide §4.B: "File
PDF baru akan di-generate secara instan saat pelari mengklik tombol
Download"). It lives in the Public Portal, §5.

### 4.4 Media Gallery & OCR

| Method & Path | Role | Purpose |
|---|---|---|
| `POST /api/v1/events/{id}/albums` | Staff | Create album |
| `GET /api/v1/events/{id}/albums` | Staff | List |
| `PATCH /api/v1/albums/{aid}` | Staff | Update (name, `is_public`) |
| `DELETE /api/v1/albums/{aid}` | Admin | Delete |
| `POST /api/v1/albums/{aid}/photos/upload-urls` | Staff | Body: array of `{filename, content_type}`. Handler loops `StorageService.RequestUpload` once per entry (private bucket) — one HTTP round trip for the photographer, N ordinary Phase 0 calls underneath. Returns `[{storage_id, upload_url}]`. |
| `POST /api/v1/albums/{aid}/photos/complete` | Staff | Body: array of `storage_id`s (from `upload-urls` above) the client finished PUTting — confirmed via `StorageService.CompleteUpload`/`CompletePut` same as Phase 0. Creates one `photos` row per id (`original_storage_id` set, `ocr_status=pending`) and enqueues one `media.photo_process` job (thumbnail + watermark + OCR per photo, progress tracked via `jobs.progress_current/total`). The thumbnail job itself creates a *new* `objects` row (public bucket) once it renders the thumbnail, and sets `photos.thumbnail_storage_id`. Returns `202 {job_id}` |
| `GET /api/v1/albums/{aid}/photos` | Staff | List, paginated, filter `ocr_status` |
| `DELETE /api/v1/photos/{pid}` | Staff | Delete a photo |
| `GET /api/v1/albums/{aid}/photos/needs-review` | Staff | Filters `ocr_status=failed` or below a confidence threshold — the guide's "Manual Tagging/Correction" dashboard queue |
| `GET /api/v1/photos/{pid}/tags` | Staff | List detected/manual tags |
| `POST /api/v1/photos/{pid}/tags` | Staff | Manual tag (`source=manual`) — the correction workflow |
| `DELETE /api/v1/photos/{pid}/tags/{tagId}` | Staff | Remove a wrong tag |

### 4.5 Jobs (generic, shared by all three async flows above)

| Method & Path | Role | Purpose |
|---|---|---|
| `GET /api/v1/jobs/{id}` | Staff | Status, progress, `result`/`error` |
| `GET /api/v1/jobs` | Staff | List recent jobs, paginated, filter `type`/`status` |

## 5. Public Runner Portal (no auth)

Mounted with **no** `requireUserAuth`/`requireM2MAuth` in the chain, same
posture as Storage's data-plane routes — but IP rate-limited
(`middleware.RateLimit`, same helper already used for login/`oauth/token`)
since there is no credential at all to key off. This is the module most
worth getting review on before coding, because "no login" plus "search by
BIB or name" is also, unavoidably, an enumeration surface over runners'
PII (name, times, photos) — see the open decision in §7.

| Method & Path | Purpose |
|---|---|
| `GET /api/v1/public/events/{eventSlug}` | Public event info (name, venue, dates, logo). 404s unless `status=published` |
| `GET /api/v1/public/events/{eventSlug}/search?q=` | Query is a `bib_number` or a name fragment; `WHERE bib_string = ? OR bib_number = ?` for exact BIB, `ILIKE` for name — exactly the guide's §4.C clause. Returns minimal match list (name + bib only, no PII beyond what's needed to disambiguate) |
| `GET /api/v1/public/events/{eventSlug}/participants/{bibNumber}` | Full personalized dashboard: gun/net time, overall/category rank, cert + photo-count summary |
| `GET /api/v1/public/events/{eventSlug}/participants/{bibNumber}/certificate` | **On-demand** PDF generation (guide's <3s KPI, §6.2). First hit renders and caches to the private bucket; subsequent hits serve the cached object — the guide's own "Lazy Load / On-Demand Generation" mitigation |
| `GET /api/v1/public/events/{eventSlug}/participants/{bibNumber}/photos` | Paginated `photo_tags` join, only photos in an `is_public=true` album |

This needs one new middleware, `ResolvePublicEvent` (`internal/httpapi/
middleware`): extracts `eventSlug`, looks up the event (read-only,
`published` only), and injects `tenant_id` into context the same shape
`RequireTenantForM2M` does — so every downstream repository call still
goes through `WithTenantTx` / RLS unchanged. No membership check, because
there is no principal to check membership for.

## 6. Async job architecture

Builds exactly what the README already committed to, now that there's a
real job to build it against:

- `internal/platform/jobqueue` (new): thin wrapper over the existing
  `internal/platform/rediscli` client — `Enqueue(type, payload) (jobID,
  error)` pushes to a Redis list and inserts the `jobs` row in one
  `WithTenantTx`; a worker `Dequeue(ctx) (*Job, error)` blocking-pops.
- `cmd/worker/main.go` (new): same wiring shape as `cmd/api/main.go` minus
  the HTTP server — config → DB/Redis → services → a poll loop that
  dequeues, runs the type-specific handler inside each module's own
  package per §9 (`participant/import`'s job handler, `generator`'s
  bulk-BIB job handler, `gallery/jobs`'s thumbnail+OCR job handlers), and
  updates the `jobs` row
  (`processing → completed`/`failed`, with `result`/`error` populated)
  inside `database.DB.WithTenantTx`, matching every HTTP handler's
  transaction pattern today.
- A failed job is left `failed` with `error` populated, not retried
  automatically — Phase 1's KPI bar (§6 of the guide) is about OCR
  accuracy and E-Cert latency, not queue resilience; a retry policy is a
  reasonable Phase 1.1 follow-up once real failure modes are observed,
  not something to guess the shape of upfront (the same reasoning the
  README already gave for not building the queue before Phase 0 ended).

## 7. Open decisions (flagged, not blocking the endpoint contracts above)

These affect what runs *inside* the `media.photo_process` and
`generator.bib_batch` job handlers, not any route's shape, so they don't
block starting on §2–§6. Worth a explicit call before those two handlers
are written:

1. ~~OCR engine~~ **Settled**: bib detection runs as an external service
   already deployed on Google Cloud Run — not built in this repo. Phase 1
   adds a thin HTTP client (`internal/platform/ocrclient`, §9) that the
   `media.photo_process` job handler calls per photo.
   **Contract, also settled:** the worker mints a presigned GET URL for
   the photo (`internal/platform/objectstorage`, same mechanism the
   private-bucket download endpoint already uses) and sends *that URL* to
   the Cloud Run service, which fetches the image itself — no image bytes
   pass through the worker. The service is called with **no auth header**
   (currently a public endpoint) — `ocrclient.go` is written with the
   caller-auth step as a no-op today, structured so a static API key or a
   Cloud Run IAM ID token can be dropped in later (an `OCR_SERVICE_URL`
   env var either way; `OCR_SERVICE_AUTH_MODE=none` for now) without
   touching any call site. Response is assumed to be
   `{bib_string, bib_number, confidence_score}` per photo — reconfirm the
   exact field names/shape against the real service before wiring
   `phototag_repo.go`'s insert.
2. **SVG→PDF rendering for BIB/E-Cert.** The guide suggests
   Puppeteer/wkhtmltopdf/PDFKit — none are Go-native. Likely path: shell
   out to a headless-Chromium or `rsvg-convert`/`wkhtmltopdf` binary
   available in the container image (`Dockerfile` already builds on
   `alpine`, where these are installable packages), rather than pulling a
   Go rendering dependency of uncertain reachability. Needs confirming
   against this sandbox's actual package-install access before `cmd/api`
   depends on it.
3. **Participant/race deletion semantics.** `DELETE .../participants/{id}`
   above is a hard delete for Phase 1's simplicity; once results/photos
   reference a participant row (Phase 1 already does, via `bib_number`),
   a soft-delete/status flag may be worth switching to before Phase 2 adds
   payments — flagging now so it isn't a silent behavior change later.

## 8. Suggested build order

1. `0005`–`0010` migrations (storage-provider retrofit first) + `internal/domain` types (foundation everything else sits on).
2. Events & Races (§4.1) — no async, proves the tenant-scoped CRUD pattern extends cleanly to a new resource.
3. `internal/platform/jobqueue` + `cmd/worker` skeleton with one trivial job type, to de-risk the queue mechanics before real handlers depend on it.
4. Participants + CSV import (§4.2) — the guide's own P0 priority, and the first real job handler.
5. Generator (§4.3) once a template format is settled (open decision §7.2).
6. Media Gallery & OCR (§4.4) once the OCR engine is settled (open decision §7.1).
7. Public Runner Portal (§5) last, since it reads data every prior module produces.

## 9. Planned folder structure — package-per-bounded-context

Superseded the earlier layered sketch (one `*_repo.go`/`*_service.go` per
resource dropped into the shared `repository`/`service` packages) - this
section originally proposed that Phase 1's five modules (Event,
Participant, Gallery, Generator, Portal) would be the first thing in this
codebase to move to a package-per-bounded-context shape, while Phase 0
stayed on its original layered shape underneath. **That premise is now
out of date**: `docs/phase0-refactor-plan.md` carried Phase 0 itself over
to exactly this shape first (`internal/auth`, `internal/tenant`,
`internal/oauthclient`, `internal/storage`, each owning its entity/
repository/service/handler/routes together). Phase 1's five modules
should simply follow that already-proven precedent - entity, repository,
service, and HTTP handler live together in one package per module,
because nothing outside a module shares its invariants (e.g. "`bib_number`
unique within an Event" is `participant`'s alone to enforce) - rather than
be the first modules to establish the pattern.

**Two wiring problems this section originally set out to solve. Both are
already solved by the Phase 0 refactor - nothing left to design, just
precedent to follow.**

*Problem 1 — routing* (already solved). The concern: if `event`/
`participant`/... each register their own routes (so a module is
genuinely self-contained, rather than split across its own package plus a
`routes_event.go` living elsewhere), those packages need the shared
per-request middleware chain, but `router.go` also needs to import them
to wire everything up - a two-way import Go rejects. `docs/
phase0-refactor-plan.md`'s Step 1 already extracted exactly this into a
leaf package both sides depend on downward:

```
internal/httpapi/routing/routing.go
    type Middlewares struct { RequireUserAuth, RequireM2MAuth, RequireTenantForUser, ... func(http.Handler) http.Handler }
    func Chain(h http.HandlerFunc, mws ...func(http.Handler) http.Handler) http.Handler
```

Every Phase 0 bounded context already registers its own routes this way
(`auth.RegisterRoutes(mux, mw, ...)`, `tenant.RegisterRoutes(mux, mw,
...)`, `oauthclient.RegisterRoutes(mux, mw, ...)`, `storage.RegisterRoutes
(mux, mw, ...)`, each called from `internal/httpapi/router.go`'s
`NewRouter`). `router.go` today is already exactly this shape: build
`routing.Middlewares` from the `internal/httpapi/middleware` constructors
(that part still needs the DB/repos, so it stays in `router.go`), then
call each bounded context's `RegisterRoutes`. Phase 1 modules add one
more call each, in the same place:

```go
// router.go, Phase 1 addition
mw := routing.Middlewares{ /* ... same fields as today's mws, exported ... */ }
event.RegisterRoutes(mux, mw, d.Event)
participant.RegisterRoutes(mux, mw, d.Participant)
gallery.RegisterRoutes(mux, mw, d.Gallery)
generator.RegisterRoutes(mux, mw, d.Generator)
portal.RegisterRoutes(mux, mw, d.Portal)   // portal uses only the public-facing subset (rate limit + ResolvePublicEvent)
```

*Problem 2 — composition root.* `internal/app.Build()` is already Phase
0's single composition root — its own doc comment says why:
"so `cmd/api/main.go` and the HTTP integration test share exactly one
wiring path instead of the test reimplementing (and risking drifting
from) what production actually runs." `cmd/worker/main.go` must **not**
become a second place that constructs `database.Open`, `rediscli.New`,
every repository and every service from scratch — that's exactly the
drift `internal/app` exists to prevent, and Phase 1 has five new modules'
worth of construction to get wrong twice. So: `app.Build` is extended to
also construct each module's `Service` (`event.NewService(appDB)`,
`participant.NewService(appDB)`, `gallery.NewService(appDB, storageService,
queue)`, `generator.NewService(appDB, storageService, pdfRenderer)`,
`portal.NewService(participantSvc, gallerySvc, generatorSvc)`,
`jobqueue.New(redisClient, appDB)`), and `httpapi.Deps` gains matching
fields (`Event *event.Service`, `Participant *participant.Service`, ...).
Both binaries then call the *same* `app.Build(ctx, cfg, log)` and differ
only in what they do with the one `*app.App` it returns:

```go
// cmd/api/main.go — unchanged shape, Deps just carries more now
a, _ := app.Build(ctx, cfg, log)
srv.Handler = httpapi.NewRouter(a.Handler)   // NewRouter calls each module's RegisterRoutes

// cmd/worker/main.go — same app.Build call, different consumer
a, _ := app.Build(ctx, cfg, log)
dispatcher := jobqueue.NewDispatcher()
participantimport.RegisterJob(dispatcher, a.Handler.Participant)
gallery.RegisterJobs(dispatcher, a.Handler.Gallery)          // thumbnail + ocr
generator.RegisterJob(dispatcher, a.Handler.Generator)        // bib batch
worker.Run(ctx, a.Handler.JobQueue, dispatcher)               // poll loop
```

Neither binary constructs a repository or a service by hand — both are
pure consumers of the one graph `app.Build` assembles, exactly like
`cmd/api/main.go` is today. `test/integration` keeps calling `app.Build`
unchanged too, so Phase 1's integration tests get this wiring for free,
the same guarantee Phase 0's tests already have.

```
racetify-api/
├── cmd/
│   ├── api/, migrate/                       (existing, unchanged)
│   └── worker/main.go                       NEW — calls the SAME internal/app.Build(cfg) as cmd/api;
│                                             registers each module's job handler into a jobqueue.Dispatcher,
│                                             then polls. Builds nothing itself — no second composition root.
│
├── migrations/
│   ├── 0005_storage_providers.{up,down}.sql  Phase 0 retrofit — objects.provider, runs first
│   ├── 0006_events_races.{up,down}.sql
│   ├── 0007_participants.{up,down}.sql
│   ├── 0008_generator_templates.{up,down}.sql
│   ├── 0009_media_gallery.{up,down}.sql
│   └── 0010_jobs.{up,down}.sql                schema shape independent of Go package layout, per §2
│
├── internal/
│   ├── auth/ tenant/ oauthclient/ storage/    PHASE 0 bounded contexts — stays exactly as-is; each already
│   │                                          entity+repository+service+handler+routes-in-one-package, per
│   │                                          docs/phase0-refactor-plan.md
│   ├── domain/ security/ mailer/ platform/    PHASE 0 — stays exactly as-is; domain now holds only the
│   │                                          shared sentinel errors (errors.go) — every entity that used
│   │                                          to live here moved into its owning bounded context above
│   ├── repository/                            PHASE 0 — stays exactly as-is; holds only the two
│   │                                          cross-bounded-context integration tests
│   │                                          (pagination_integration_test.go, rls_isolation_test.go),
│   │                                          no production code
│   │
│   ├── httpapi/                              PHASE 0, one addition
│   │   ├── handlers/ respond/ reqctx/ router.go routes_health.go     unchanged (health check is the one
│   │   │                                                              endpoint with no bounded context)
│   │   ├── middleware/
│   │   │   └── public_tenant.go              NEW — ResolvePublicEvent (§5): event_slug → tenant ctx, no membership check
│   │   └── routing/                          PHASE 0, already exists — Middlewares/Chain, see §9 intro above
│   │       └── routing.go
│   │
│   ├── event/                                 NEW — bounded context
│   │   ├── event.go  race.go                  entities (Race has no invariant of its own — EO manages both together)
│   │   ├── repository.go  service.go
│   │   ├── handler.go  dto.go
│   │   └── routes.go                          RegisterRoutes(mux, routing.Middlewares, Deps)
│   │
│   ├── participant/                           NEW — bounded context
│   │   ├── participant.go                     entity + the real invariant: bib_number unique within an Event
│   │   ├── repository.go                      List/Get/BulkUpsert/Search(bib_or_name)
│   │   ├── service.go                         enforces uniqueness; exposes Search for portal/ to call
│   │   ├── import/                             CSV ingestion sub-package (imports participant, not the reverse — no cycle)
│   │   │   ├── column_mapping.go  importer.go   parse → map → validate dedup → BulkUpsert
│   │   │   └── job.go                           the participants.import job handler cmd/worker dispatches to
│   │   ├── handler.go  dto.go  routes.go
│   │
│   ├── gallery/                                NEW — bounded context (Album → Photo → PhotoTag cluster)
│   │   ├── album.go  photo.go  phototag.go
│   │   ├── repository.go  service.go           upload-URL batching + complete-upload orchestration, enqueues jobs
│   │   ├── jobs/
│   │   │   ├── thumbnail_job.go                 resize 720p + watermark (internal/platform/imaging)
│   │   │   └── ocr_job.go                       calls internal/platform/ocrclient (presigned URL in, tags out)
│   │   ├── handler.go  dto.go  routes.go
│   │
│   ├── generator/                              NEW — bounded context (BIB + E-Certificate)
│   │   ├── template.go                          SVG template entity
│   │   ├── renderer.go                          {{BIB_NUMBER}}/{{RUNNER_NAME}} replace + PDF render
│   │   │                                        (internal/platform/pdfrender; §7.2 still open)
│   │   ├── service.go                           bulk BIB-batch job handler + on-demand cert render/cache
│   │   ├── repository.go  handler.go  dto.go  routes.go
│   │
│   ├── portal/                                 NEW — the frictionless, no-login Runner dashboard
│   │   ├── dashboard.go                         composes participant.Service + gallery.Service + generator.Service
│   │   │                                        (portal is the composition root here — imports the other four,
│   │   │                                        none of them import portal back, so no cycle)
│   │   └── handler.go  routes.go                GET /api/v1/public/events/{slug}/... — routes.go wires ONLY
│   │                                            rate-limit + ResolvePublicEvent, no user/M2M auth middleware
│   │
│   ├── jobqueue/                               NEW — top-level (app-orchestration, not raw infra like objectstorage/rediscli)
│   │   └── queue.go                            Enqueue (Redis list push + jobs-row insert, WithTenantTx) +
│   │                                           a Dispatcher registry cmd/worker populates per job type —
│   │                                           jobqueue itself imports no bounded-context package, so every
│   │                                           module can depend on jobqueue with nothing importing back
│   │
│   └── platform/                               PHASE 0 package — one Phase 0 file redesigned, three Phase 1 leaves added
│       ├── objectstorage/                      CHANGED — NewDriver(cfg) Driver → NewRegistry(cfg) *Registry (see §2's
│       │                                       multi-provider subsection). local_driver.go/r2_driver.go unchanged;
│       │                                       a future gdrive_driver.go slots in beside them, ProxyDriver-shaped
│       ├── rediscli/ database/ logger/         unchanged
│       ├── ocrclient/                          NEW — HTTP client for the Cloud Run bib-detection service (§7.1:
│       │                                       presigned GET URL in, {bib_string,bib_number,confidence} out, no auth today)
│       ├── pdfrender/                          NEW — SVG→PDF (§7.2, still open)
│       └── imaging/                            NEW — resize + watermark
│
├── test/integration/
│   ├── event_test.go  participant_test.go  generator_test.go  media_test.go
│   └── public_portal_test.go                   includes a cross-tenant/unpublished-event 404 proof —
│                                               the §5 equivalent of Phase 0's cross-tenant 403 proofs
│
├── .env.example                                + JOBQUEUE_*, OCR_SERVICE_URL/OCR_SERVICE_AUTH_MODE, WATERMARK_*
└── docs/
    ├── openapi.yaml                            extended with every Phase 1 path
    └── phase1-api-plan.md                      this file
```

**Import direction, spelled out** (so nothing above accidentally cycles):
`event`, `participant`, `gallery`, `generator` each depend only downward —
on `internal/domain` (sentinel errors), `internal/platform/*`,
`internal/httpapi/{routing,middleware,respond,reqctx}`, and
`internal/jobqueue`; `gallery` and `generator` additionally depend on
`internal/storage.Service` (Phase 0, unchanged) for every
`storage_id` they hand out or resolve — the same package `internal/
tenant` and `internal/oauthclient` already depend on today via
`internal/app.Build()`'s wiring, so this isn't a new kind of edge in the
graph, just one more caller of it. `participant/import` and
`gallery/jobs` depend upward one level into their own parent package
only. `portal` is the one module allowed to depend sideways, on the other
four's exported `Service` types, because it's a read-only composition
layer with nothing depending on it in turn.

There is exactly **one** composition root: `internal/app.Build()`. It's
the only place in the codebase that imports every module to *construct*
it. `router.go` (inside `httpapi.NewRouter`) and `cmd/worker/main.go` each
import every module too, but only to *wire already-built services to an
entry point* — HTTP routes for one, job-dispatch registration for the
other — never to construct a repository, a DB connection, or a service
themselves. That distinction is the whole point of §9's Problem 2: two
files touching every module is fine; two files each independently
*building* every module is the drift Phase 0's `internal/app` doc comment
already warns against.
