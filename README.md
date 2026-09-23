# Racetify API — Phase 0 (Fondasi Sistem)

Multi-tenant race-event management platform for Indonesian running events
(road running, trail running, ultra trail). This repository is the
**Phase 0** foundation described in `implementation_guide_phase_0.md` and
`racetify_prd_final.md`: centralized user auth & registration, tenant
(organizer) onboarding, staff invitation, RBAC, and the Server-to-Server
OAuth 2.0 Client Credentials grant for M2M integrations. Business-domain
endpoints (events, races, participants, results, ...) are out of scope and
ship in later phases.

Stack: **Go 1.24 · PostgreSQL 16 (Row-Level Security) · Redis 7**.

## Contents

- [Quick start](#quick-start)
- [Architecture](#architecture)
- [Multi-tenancy & Row-Level Security](#multi-tenancy--row-level-security)
- [Auth & session design](#auth--session-design)
- [RBAC](#rbac)
- [No database-level enums](#no-database-level-enums)
- [Social login identity](#social-login-identity)
- [Pagination](#pagination)
- [Object Storage](#object-storage)
- [API surface](#api-surface)
- [Testing](#testing)
- [Configuration](#configuration)
- [Docker](#docker)
- [CI/CD](#cicd)
- [Deliberate deviations from the implementation guide](#deliberate-deviations-from-the-implementation-guide)
- [Known Phase 0 limitations](#known-phase-0-limitations)
- [Looking ahead: Phase 1 async jobs](#looking-ahead-phase-1-async-jobs)

## Quick start

Requires Go 1.24+, PostgreSQL 16+, and Redis 7+ (or Docker).

```bash
cp .env.example .env
make docker-up        # Postgres + Redis + migrate + api, all containerized
```

or, against a local Postgres/Redis already listening on `localhost`:

```bash
cp .env.example .env
make migrate          # applies migrations/*.sql via DATABASE_MIGRATOR_URL
make run              # starts the API on :8080 (go run ./cmd/api)
```

Health checks: `GET /healthz` (liveness), `GET /readyz` (checks Postgres +
Redis connectivity).

The full HTTP surface is documented in [`docs/openapi.yaml`](docs/openapi.yaml)
(OpenAPI 3.0.3, validated with `openapi-spec-validator`).

## Architecture

```
cmd/
  api/          entrypoint: wires config → DB/Redis → services → router → http.Server
  migrate/      entrypoint: applies migrations/*.sql via the migrator DSN only
internal/
  app/          shared wiring shared between cmd/api and the integration tests
  config/       env/.env loading
  domain/       entities + sentinel errors, no framework/DB imports
  repository/   Postgres access (lib/pq), tenant-scoped repos accept a tenant context;
                pagination.go holds the shared keyset-pagination convention (see
                "Pagination" below)
  security/     password hashing, JWT, opaque tokens, UUIDs
  service/      business logic: auth, tenant, invitations, oauth clients, storage, rate limiting
  mailer/       Mailer interface (LogMailer implementation for Phase 0)
  platform/
    database/     *sql.DB wrapper, WithTx/WithTenantTx, migration runner
    rediscli/     hand-rolled Redis client (see "Deliberate deviations" below)
    objectstorage/ presigned-URL, encrypted-at-rest Object Storage backend (see
                   "Object Storage" below)
    logger/       slog setup
  httpapi/
    handlers/   HTTP handlers, request/response DTOs
    middleware/ auth, tenant resolution, RBAC, rate limiting, observability
    reqctx/     typed context accessors (authenticated principal, resolved tenant)
    respond/    {"data"|"error"} envelope + domain-error → HTTP status mapping
    router.go   Deps, NewRouter, the shared middleware set, and the chain() helper
    routes_*.go one file per resource (auth, tenant, oauth, storage, resource,
                health) registering that resource's routes onto the shared mux -
                split out so one resource's routes never collide in a diff with
                another's
migrations/     numbered .up.sql/.down.sql pairs, embedded into the migrate binary
docs/
  openapi.yaml  hand-authored OpenAPI 3.0.3 spec
test/integration/  full HTTP-level flow tests (build tag `integration`)
```

Request flow: `net/http` → middleware chain (observability → auth →
tenant resolution → RBAC → rate limit, as each route needs) → handler →
service → repository → `database.DB.WithTx`/`WithTenantTx`, which opens a
transaction, sets the Postgres session variable Row-Level Security
policies key off, and commits/rolls back around the handler's unit of work.

## Multi-tenancy & Row-Level Security

Racetify uses a **shared database, shared schema** model. Every
tenant-scoped table carries a `tenant_id` column, and Postgres
**Row-Level Security** policies filter every read/write to rows matching
`current_setting('app.tenant_id', true)` — a session-local variable set
with `SELECT set_config('app.tenant_id', $1, true)` at the start of each
transaction (`database.DB.WithTenantTx`). No `WHERE tenant_id = ...` clause
in application code is trusted to enforce isolation; the database does.

Three Postgres roles are used deliberately (see `internal/config/config.go`
doc comments and `migrations/0003_row_level_security.up.sql`). The app/admin
role *names* are generic on purpose, not hardcoded to this product's name —
they're an operational deployment detail, driven by `DB_APP_ROLE`/
`DB_APP_ROLE_PASSWORD`/`DB_ADMIN_ROLE`/`DB_ADMIN_ROLE_PASSWORD` env vars
(`.env.example`) and templated into the migration SQL at apply time
(`internal/platform/database.MigrationSet.Vars`, built by
`cmd/migrate/main.go` with identifier validation + password escaping —
never raw string interpolation):

| Role (dev default) | Used by | Privileges |
|---|---|---|
| `app_user` (`DB_APP_ROLE`) | the running API server, for almost everything | subject to RLS |
| `platform_admin` (`DB_ADMIN_ROLE`) | a handful of narrow, documented pre-tenant-context lookups (e.g. resolving which tenants a user belongs to, before a tenant is selected) | `BYPASSRLS` |
| migrator (Postgres bootstrap superuser in local dev) | `cmd/migrate` only, never the running server | DDL / `CREATE ROLE` |

The running API process **never** has DDL privileges and never runs
migrations itself — that's a physical guarantee, not a convention:
`cmd/api/main.go` only ever opens `DATABASE_URL` and `DATABASE_ADMIN_URL`.

In `docker-compose.yml`, `migrate` and `api` build their `DATABASE_URL`/
`DATABASE_ADMIN_URL`/`DB_APP_ROLE`/`DB_ADMIN_ROLE` values from Compose's own
native `${VAR}` substitution (sourced from the `.env` file in this
directory - the same one `cp .env.example .env` creates and
`internal/config`'s `loadDotEnv` reads for `go run`/`make`), so both
services stay in sync from one place instead of duplicating the literal
password across two service blocks. The same goes for the database name
and the Postgres bootstrap superuser: `postgres`'s own
`POSTGRES_DB`/`POSTGRES_USER`/`POSTGRES_PASSWORD` and `migrate`'s
`DATABASE_MIGRATOR_URL` both derive from `DB_NAME`/`DB_MIGRATOR_USER`/
`DB_MIGRATOR_PASSWORD` (Compose-only - not read by the Go binaries, which
just take each `DATABASE_*_URL` as one whole DSN). `api`'s `APP_ENV`,
`HTTP_PORT`, `JWT_SECRET`, `JWT_ISSUER` and `CORS_ALLOWED_ORIGINS` are
substituted the same way, straight from the matching `.env.example` var of
the same name.

None of these substitutions carry a `${VAR:-default}` fallback - the dev
defaults live once, in `.env.example`, not a second time as inline
fallbacks inside `docker-compose.yml` too. That means `.env` has to exist
and be complete (the Quick start's `cp .env.example .env` covers this) -
if a var is missing, Compose silently substitutes an empty string rather
than failing, which surfaces as a broken connection string or an empty
`HTTP_PORT`/`JWT_SECRET`, not a clear error. `REDIS_ADDR` is the one
exception left hardcoded in `docker-compose.yml`: it has to be the
Docker-internal hostname `redis:6379`, not `.env.example`'s host-based
`localhost:6379`, so substituting it from `.env` would silently break
container-to-container networking.

This is Compose's own substitution, separate from `internal/config`'s
`.env` loader (`loadDotEnv`) used by `go run`/`make` locally, which does
not do `${VAR}` expansion - keep `DATABASE_URL`/`DATABASE_ADMIN_URL` and
`DB_APP_ROLE`/`DB_APP_ROLE_PASSWORD`/`DB_ADMIN_ROLE`/`DB_ADMIN_ROLE_PASSWORD`
consistent by hand in your local `.env` (see its comments). Compose does
*not* url-encode its substitution either, so a generated password used
there should stay alnum/underscore.

Tenant isolation is proven three ways:
1. Manually with raw `psql`, setting `app.tenant_id` and confirming a
   session only sees its own tenant's rows (and zero rows with no context
   set).
2. `internal/repository/rls_isolation_test.go` (`TestTenantIsolation`,
   build tag `integration`) — the same proof, automated at the repository
   layer. This directory now holds only this cross-bounded-context
   integration test and `pagination_integration_test.go` - see either
   file's doc comment for why they don't belong to any single bounded
   context's own package.
3. `test/integration/http_test.go` (`TestFullPhase0Flow`) — end to end
   over real HTTP, including a demo protected resource
   (`GET /api/v1/tenant/summary` for a user session,
   `GET /api/v1/m2m/tenant/summary` for an M2M token) that a second
   tenant's caller gets `403` on.

## Auth & session design

**Password hashing:** PBKDF2-HMAC-SHA256, 210,000 iterations (see
"Deliberate deviations" — the guide specifies Argon2id/bcrypt).

**User sessions:** short-lived JWT access token (default 20m, HS256) +
opaque, non-JWT refresh token. The refresh token is stored **hashed**
(SHA-256) in Postgres, delivered to the client as an HTTP-only cookie
scoped to `/api/v1/auth`, and rotated on every use. Reusing an
already-rotated refresh token is treated as token theft: it revokes the
entire session family and returns `401`.

**M2M sessions:** a single scoped JWT (`token_type: m2m_access`) issued by
the Client Credentials grant, with `tid` (tenant_id) and `scopes` claims
burned in at grant time — a client's tenant and scopes can never be
widened by presenting a different header, only by re-authenticating.

**Revocation:** access tokens carry a `jti`; logout adds it to a Redis
blacklist (`internal/security/jwt.go`, checked by
`internal/httpapi/middleware/auth.go`) with a TTL matching the token's own
remaining lifetime, so the blacklist entry never outlives the token.

**Rate limiting:** Redis fixed-window counters (`INCR` + `EXPIRE`) guard
`/api/v1/auth/login` (per source IP) and `/oauth/token` (per source IP
*and* per `client_id`, so one throttled client can't be starved by another).

**Google Sign-In:** Authorization Code flow, implemented with `net/http`
directly (see "Deliberate deviations"). Disabled (`501`) unless
`GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET`/`GOOGLE_REDIRECT_URL` are all set
— it was implemented but not live-tested against real Google credentials
in this environment.

## RBAC

`internal/tenant.MemberRole` (a type alias for `internal/platform/rbac.
MemberRole` - see that package's doc comment) defines `owner > admin >
staff` with an `IsAtLeast()` hierarchy check, enforced per-route by
`internal/tenant.RequireRole`. A Super Admin flag on the user
record bypasses tenant-scoped checks for platform-level operations. M2M
callers are authorized by the scopes burned into their access token, not
by a member role.

**Platform Verification (Super Admin tenant status):**
`PATCH /api/v1/admin/tenants/{id}/status` implements the PRD's "verifikasi
organizer" capability - a Platform Super Admin approves (`active`), rejects,
or suspends a tenant. Gated twice: `middleware.RequireSuperAdmin` at the
router (coarse admission control) and `actorIsSuperAdmin` re-checked inside
`TenantService.SetTenantStatus` (the real enforcement), the same
double-gating pattern every other role check in this codebase uses.
Rejecting or suspending requires a non-empty `reason`, stored on
`tenants.status_reason` and cleared on reactivation, so a Tenant Owner can
be shown *why* without needing audit-log access; the full history is also
in `audit_logs` (`tenant.status_changed`).

## No database-level enums

Status/role/bucket-style columns (`users.status`, `tenants.status`,
`tenant_members.role`, `objects.bucket`, ...) are plain `TEXT` with **no**
`CHECK` constraint enumerating their legal values, and no native Postgres
`ENUM` type either - both require a schema migration to add or rename a
value later. The legal set is defined exactly once, in Go
(`internal/domain`'s `*Status`/`*Role` types), which is also the only place
that ever writes these columns: every write in this codebase goes through
the service layer, never raw/external input. See
`migrations/0001_core.up.sql`'s doc comment for the same reasoning at the
source.

## Social login identity

Google (and any future provider) identities live in their own table,
`user_social_accounts(user_id, provider, provider_user_id)`, rather than a
`google_id` column on `users` - a second provider needs a new row, not a
new `users` column, and a user linking multiple providers is naturally
multiple rows. `auth.Service.LoginOrRegisterWithGoogle` creates a new user's
`users` row and its `user_social_accounts` row inside one transaction,
which is what now enforces "must have a password or a linked social
account" (a `CHECK` constraint can't span two tables). See
`internal/auth/social_account.go` and its `Repository`'s
user_social_accounts section (`internal/auth/repository.go`).

## Pagination

Every `List` endpoint (`GET /api/v1/members`,
`.../invitations`, `.../oauth-clients`, `.../storage/objects`) uses
**cursor (keyset) pagination**, not `OFFSET`/`LIMIT` — a deliberate Phase 0
foundation choice, not a Phase 1 afterthought: `Participant`/`Photo`
records are expected at "puluhan ribu"/"ribuan" row scale with concurrent
bulk inserts (CSV import, live photo uploads), and `OFFSET` pagination
skips/repeats rows under concurrent writes and re-scans discarded rows as
the offset grows. Every response is
`{"data": {"items": [...], "next_cursor": "..."}}`; `next_cursor` is
omitted once there is no further page. Request the next page with
`?cursor=<next_cursor>`; `?limit=` defaults to 20 and is clamped to 100.
The cursor is opaque (base64 of `created_at|id`) — treat it as such, its
encoding is not part of the API contract.

See `internal/repository/pagination.go` for the `Page[T]`/`PageParams`
convention and `internal/repository/pagination_integration_test.go` for a
proof that paging through rows sharing an identical `created_at` (the
realistic bulk-import case) skips or duplicates nothing.

## Object Storage

Implements the implementation guide's §3/§6 Object Storage deliverable —
presigned upload/download URLs against files encrypted at rest — behind a
swappable **driver** (`internal/platform/objectstorage`), selected by the
`STORAGE_DRIVER` env var: the same "disk"/"driver" pattern Laravel's
`Storage` facade (`config/filesystems.php`'s `disks`, picked by
`FILESYSTEM_DISK`) and AdonisJS's `Drive` (`config/drive.ts`'s `services`,
picked by `DRIVE_DISK`) use — one interface, several backends, chosen by a
config value rather than a compile-time choice.

Unlike an earlier version of this driver, `local` and `r2` do **not** share
one contract — each is native to its own backend on purpose:

- **`local`** (default, a `ProxyDriver`): every upload/download is proxied
  through this API's own `PUT`/`GET /api/v1/storage/objects/{bucket}/
  {tenantId}/{key}` endpoint. Presigned URLs are HMAC-SHA256-signed and
  time-limited (`presign.go`'s `presigner`, our own scheme, not S3 SigV4),
  and every object is encrypted at rest with AES-256-GCM (`crypto.go`)
  before it reaches disk. Zero external account needed — the right call for
  local dev or an actually self-hosted deployment.
- **`r2`** (a `DirectDriver`): real Cloudflare R2/S3 native presigned URLs
  (SigV4, via `github.com/aws/aws-sdk-go-v2/service/s3`'s
  `*s3.PresignClient`), pointing straight at the bucket — or any
  S3-compatible bucket, including real AWS S3 (see `.env.example`'s
  `STORAGE_R2_ENDPOINT`). The client PUTs/GETs bytes directly to/from R2;
  this server never sees them, so there is no app-level AES-256-GCM step
  for this driver — R2's own at-rest encryption is relied on instead.

Both drivers still share: the same **two-bucket split** (`public` —
tenant logos, photo thumbnails, meant to sit behind a CDN; `private` — raw
CSVs, high-res photo originals, BIB/certificate PDFs, served only via a
time-limited presigned URL); the same **control-plane API**
(`RequestUpload`/`RequestDownload`/`List`); and the same **two-layer
cross-tenant defense** — `RequestDownload` only ever mints a URL after a
tenant-scoped, RLS-backed lookup confirms the object belongs to the
caller's own tenant, and (for `local`) a leaked/forged HMAC signature can't
be reused across tenants or keys either
(`objectstorage_test.go`'s `TestPresignedURLsAreTenantAndKeyScoped`, proven
again end-to-end over HTTP in `test/integration/storage_test.go`).

Where they diverge is what has to happen **after** the client PUTs bytes to
the presigned upload URL:

- **`local`**: nothing extra. Our own PUT handler receives the bytes,
  encrypts and writes them, and marks the object `'stored'` in the same
  request (`StorageService.CompletePut`).
- **`r2`**: the client must call `POST .../storage/objects/complete-upload`
  once the direct-to-R2 PUT finishes. This server never observed that PUT,
  so `StorageService.CompleteUpload` HEADs the object on R2
  (`DirectDriver.ConfirmUpload`) to confirm it actually landed and learn
  its size, before marking it `'stored'` — there is no plaintext available
  to hash, so `objects.sha256` stays `NULL` for `r2`-driver uploads (the
  column is nullable precisely for this;
  `ObjectRepository.MarkStoredTenantScoped`).

Control plane (tenant-session authenticated, `internal/httpapi/handlers/
storage_handler.go`):

| Endpoint | Who | Driver |
|---|---|---|
| `POST /api/v1/storage/objects/upload-url` | Owner/Admin | either |
| `POST /api/v1/storage/objects/complete-upload` | Owner/Admin | `r2` only — 400 on `local` |
| `GET /api/v1/storage/objects/download-url?bucket=&key=` | any active member | either |
| `GET /api/v1/storage/objects` | any active member (paginated) | either |

Data plane (unauthenticated, signature-verified, `local` only — 501 if the
active driver is `r2`, since an `r2` presigned URL points straight at the
bucket and a client should never reach this): `PUT`/`GET
/api/v1/storage/objects/{bucket}/{tenantId}/{key}`.

This split — a common `Driver` interface
(`PresignUpload`/`PresignDownload`/`PublicURL`) plus a `ProxyDriver`
extension (`VerifySignature`/`Put`/`Get`, `local`-only) and a `DirectDriver`
extension (`ConfirmUpload`, `r2`-only) — is what lets
`storage_handler.go`/`storage_service.go` stay written against one
`objectstorage.Driver` type everywhere, type-asserting to the narrower
interface only at the few call sites that actually need driver-specific
behavior, and returning a clean 400/501 rather than a panic when an
endpoint is called against the wrong driver.

**Known limitation, `local`:** `Put`/`Get` hold a whole object in memory to
encrypt/decrypt with AES-GCM (fine for Phase 0's actual file sizes — logos,
certificate templates, individual photos — capped at 25 MiB by
`StorageHandler`). Phase 1's bulk CSV import or high-resolution photo sets
would need chunked envelope encryption before removing that cap. This does
not apply to `r2`, since bytes never pass through this server at all.

**Setup for `STORAGE_DRIVER=r2`:** create one R2 bucket, mint an R2 API
token scoped to it, and fill in `.env`'s `STORAGE_R2_ACCOUNT_ID`/
`STORAGE_R2_ACCESS_KEY_ID`/`STORAGE_R2_ACCESS_KEY_SECRET`/`STORAGE_R2_BUCKET`
(see `.env.example`'s comments — one bucket holds both the `public` and
`private` buckets, namespaced `<bucket>/public|private/<tenant_id>/<key>`,
the same layout `local`'s filesystem path uses). Optionally set
`STORAGE_R2_PUBLIC_BASE_URL` to a custom domain or `r2.dev` subdomain
mapped to that bucket for a genuinely stable public-bucket URL; left blank,
`PublicURL` falls back to a long-lived (7-day) presigned URL instead, so
setup works either way. **Not exercised against a live R2 bucket in this
sandbox** — no credentials, and this environment's network egress does not
reach Cloudflare (see `objectstorage.go`'s package doc) —
`internal/platform/objectstorage/r2_driver_test.go` covers
`PresignUpload`/`PresignDownload`/`PublicURL`/`ConfirmUpload` against fake
`s3PresignAPI`/`s3HeadAPI` stand-ins instead. Please verify against a real
bucket before relying on it in production, the same caveat this README
already gives `docker-compose.yml`. A client integrating against `r2`
should also expect the asymmetric contract above (a required
`complete-upload` call after every upload) — this is unlike `local` and
should be called out in any client-facing API docs.

Adding a third driver means deciding which shape it is closer to first: a
proxy-through-our-server driver (implement `ProxyDriver`, likely embedding
`*presigner` from `presign.go` and reusing `encryptGCM`/`decryptGCM` from
`crypto.go`, the way `local_driver.go` does) or a direct-to-bucket driver
(implement `DirectDriver`, the way `r2_driver.go` does) — then add one
`case` to `NewDriver` (`objectstorage.go`). Every caller above this package
(`storage_service.go`, `storage_handler.go`, `router.go`'s
`Deps.ObjectStore`) is already written against the base `Driver` interface,
not a concrete type, and type-asserts to `ProxyDriver`/`DirectDriver` only
where it actually needs the narrower behavior.

## API surface

See [`docs/openapi.yaml`](docs/openapi.yaml) for the full spec (also
renders in any Swagger/Redoc UI). Summary:

- **Auth:** register, verify-email, login, refresh, logout, Google OAuth
  (begin + callback), `GET /api/v1/users/me`
- **Tenants:** register a tenant (caller becomes Owner), list caller's
  tenants + roles, list current tenant's members
- **Platform Verification (Super Admin):**
  `PATCH /api/v1/admin/tenants/{id}/status` — approve/reject/suspend a
  tenant, with a required reason on reject/suspend - see "RBAC" above
- **Invitations:** invite staff (Owner/Admin), list/revoke invitations,
  accept an invitation
- **OAuth Clients:** provision/list/revoke M2M credentials (Owner/Admin)
- **OAuth2:** `POST /oauth/token` — Client Credentials grant (RFC 6749 §4.4)
- **Object Storage:** provision presigned upload/download URLs, list a
  tenant's stored objects, PUT/GET the raw bytes — see "Object Storage" above
- **Demo resources:** `/api/v1/tenant/summary` and
  `/api/v1/m2m/tenant/summary` — minimal tenant-scoped endpoints that
  exist specifically to demonstrate the RLS isolation guarantee end to end

Every response body is a `{"data": ...}` / `{"error": {"code", "message"}}`
envelope (`internal/httpapi/respond`), with domain sentinel errors
(`internal/domain/errors.go`) mapped centrally to HTTP status + machine
code.

## Testing

```bash
make test              # unit tests only, no DB/Redis required
make test-integration  # full suite: RLS proof + end-to-end HTTP flow
                        # requires Postgres+Redis reachable, e.g. `make docker-up`
                        # in another terminal, or local services on the
                        # DATABASE_URL/REDIS_ADDR defaults in .env.example
```

Integration tests live behind the `integration` build tag so `go test ./...`
(and CI's unit-test step) never needs a live database. `TestFullPhase0Flow`
walks the entire Phase 0 checklist over real HTTP: registration, login,
tenant creation, staff invite + accept, RBAC enforcement (staff forbidden
from inviting), OAuth2 client_credentials grant, cross-tenant `403` for
both a user session and an M2M token, refresh rotation, refresh **reuse
detection**, logout + access-token blacklisting, and the paginated-list
envelope shape. `TestObjectStoragePutGetFlow`
(`test/integration/storage_test.go`) separately walks the Object Storage
module end to end: presign upload → PUT bytes → presign download → GET
bytes back byte-for-byte, a tampered-signature PUT rejected, a
cross-tenant download-url request failing closed at `404` before any
signature is minted, a forged cross-tenant signature rejected at `403`,
the public bucket's unsigned URL working with no signature at all, and
Staff forbidden from provisioning an upload URL (Admin+ only).

## Configuration

All configuration is environment variables, loaded from `.env` if present
(a real environment variable always wins — see `loadDotEnv` in
`internal/config/config.go`). See [`.env.example`](.env.example) for the
full list with explanations, including why three separate `DATABASE_*_URL`
values exist.

## Docker

`Dockerfile` is a multi-stage build (`golang:1.24-alpine` → `alpine:3.20`).
`docker-compose.yml` starts Postgres and Redis with health checks, runs
`cmd/migrate` as a one-shot job gated on `service_completed_successfully`,
then starts the API.

> **Caveat:** no Docker daemon was available in the sandbox this was built
> in, so the Dockerfile and compose file were written and manually
> reviewed line-by-line against the working `make run`/`make migrate`
> flow, but **not** executed. Please verify `make docker-up` on a machine
> with Docker before relying on it, and file an issue (or just fix it) if
> anything doesn't line up.

## CI/CD

`.github/workflows/ci.yml`: `gofmt -l` → `go vet` → run migrations against
a Postgres service container → `go test ./...` → `go test -tags=integration
./... -v` against Postgres+Redis service containers → build the Docker
image. A `deploy-staging` job is wired to run after a successful build on
`main`, but its actual deploy step is an honest placeholder (`echo "TODO:
..."`) — pushing to a registry and deploying needs infrastructure
(ECS/Cloud Run/Kubernetes credentials) this repository doesn't define yet.

## Deliberate deviations from the implementation guide

This was built in a sandboxed environment whose only reachable Go module
source was `github.com` via `git`/`GOPROXY=direct` — `proxy.golang.org`,
`golang.org/x/*`, `gopkg.in`, `go.googlesource.com`, and
`google.golang.org` were all unreachable. Every dependency choice below
is a direct, documented consequence of that constraint, not a silent
downgrade — each is called out again in a doc comment at its point of use.

| Guide recommends | Used instead | Why | Where |
|---|---|---|---|
| Argon2id or bcrypt | PBKDF2-HMAC-SHA256, 210k iterations | Both target packages live under `golang.org/x/crypto`, unreachable. PBKDF2-SHA256 is stdlib-only and remains an OWASP-acceptable choice at this iteration count. | `internal/security/password.go` |
| `pgx` (or similar) | `github.com/lib/pq` | `pgx/v5` pulls a Go toolchain bump and transitive deps that were unreachable; `lib/pq` is pure stdlib-adjacent with zero transitive deps. | `go.mod`, each bounded context's `repository.go`, `internal/platform/database` |
| `go-redis`/`redis-go` | Hand-rolled RESP2 client | Needs `golang.org/x/sys/cpu` and `go.uber.org/atomic`, both unreachable. Implements exactly the commands Phase 0 needs (Ping, Set, SetNX, Get, Del, Exists, Expire, Incr) over a small connection pool. | `internal/platform/rediscli/client.go` |
| `golang.org/x/oauth2` | Raw `net/http` Authorization Code flow | Package unreachable. | `internal/auth/google_oauth.go` |
| `golang-migrate` | Small hand-rolled migration runner + `go:embed` | Unreachable; the guide's requirement (versioned, repeatable SQL migrations) is met without the dependency. | `internal/platform/database/migrate.go`, `migrations/` |
| chi/gin/echo router | Go 1.22+ stdlib `http.ServeMux` (method + pattern routing) | Not a network issue — stdlib now covers Phase 0's routing needs (`GET /path/{id}` etc.) without a dependency. | `internal/httpapi/router.go` |
| AWS S3 SDK / Cloudflare R2 (originally) | `local` driver (self-hosted, HMAC-presigned, AES-256-GCM-encrypted disk) by default, **plus** an `r2` driver using the real `github.com/aws/aws-sdk-go-v2/service/s3` SDK's native presigned URLs, selected by `STORAGE_DRIVER` (see "Object Storage" above) | Originally substituted entirely: outbound HTTPS to AWS/Cloudflare's actual endpoints is still unreachable from this sandbox. The SDK's *source* turned out to be fetchable (`GOPROXY=direct` over git), so the `r2` driver was added against it - but its `PresignPutObject`/`PresignGetObject`/`HeadObject` calls are only exercised against fake `s3PresignAPI`/`s3HeadAPI` stand-ins here (`r2_driver_test.go`), never a live bucket. | `internal/platform/objectstorage/{objectstorage,local_driver,r2_driver,presign,crypto}.go` |

Two dependencies were kept because they're small, single-purpose, and
reachable with zero transitive deps: `github.com/golang-jwt/jwt/v5` and
`github.com/lib/pq`.

If this is deployed somewhere with normal module-proxy access, swapping
PBKDF2 → Argon2id and `lib/pq` → `pgx` are the two changes worth making
first; both are isolated behind small interfaces (`security.HashPassword`/
`VerifyPassword`, and the `database.Querier` interface) specifically so
that swap doesn't ripple through the codebase.

## Known Phase 0 limitations

- Google Sign-In is implemented but not live-tested (no Google OAuth
  credentials available in this environment). The Authorization Code flow
  and state-token replay protection are unit-testable in isolation but
  the guide's full round trip against Google was not exercised.
- `LogMailer` (`internal/mailer/mailer.go`) logs outgoing "emails" instead
  of sending them — verification and invitation tokens are also returned
  directly in the relevant API responses (e.g. `InvitationCreatedDTO`) so
  the flows are testable and usable without a configured mail provider.
  Swap in a real provider by implementing the `Mailer` interface, and drop
  the token from those API responses once a real provider is wired up —
  verification/reset tokens should not be user-visible via the API in
  any environment beyond local dev.
- `docker-compose.yml`/`Dockerfile` were reviewed but not executed (see
  Docker section above).
- Business-domain functionality (events, races, participant registration,
  results/timing) is explicitly out of scope for Phase 0 and not present
  here — see `implementation_guide_phase_1.md` for what's next.
- Object Storage's `local` driver's `Put`/`Get` hold a whole object in
  memory (see "Object Storage" above) — fine at Phase 0's file sizes,
  capped at 25 MiB by `StorageHandler`, but not yet chunked/streamed. Does
  not apply to `r2` (bytes never pass through this server).
- The `r2` storage driver (`STORAGE_DRIVER=r2`) is implemented and unit
  tested against fake `s3PresignAPI`/`s3HeadAPI` stand-ins, but not
  live-tested against a real Cloudflare R2 bucket — no credentials and no
  network route to Cloudflare in this sandbox (see "Object Storage"
  above).
- The `r2` driver's `objects.sha256` is always `NULL` (no plaintext ever
  reaches this server to hash) — a client that needs to verify upload
  integrity against `r2` should compute and compare its own hash, or a
  future iteration could accept a client-supplied hash on
  `complete-upload` and treat it as advisory-only (never as a substitute
  for R2's own integrity checking on the PUT itself).

## Looking ahead: Phase 1 async jobs

`implementation_guide_phase_1.md` introduces work that shouldn't run
synchronously inside an HTTP request: bulk CSV participant import, BIB/
e-certificate PDF generation, and (per the guide) OCR-based race-photo
bib-number tagging. The decision, recorded here rather than built now:

- **Redis-backed job queue** (a list-based queue over the existing
  `internal/platform/rediscli` client — no new infra) that an HTTP handler
  pushes a job onto and returns `202 Accepted` immediately, plus a
  **separate `cmd/worker` binary** that polls the queue and does the actual
  work, sharing each bounded context's service package (`internal/auth`,
  `internal/tenant`, `internal/oauthclient`, `internal/storage`) with
  `cmd/api` exactly the way `cmd/migrate` already shares `internal/
  platform/database`.
- **Why not build the queue/worker scaffolding now:** there is no real job
  to run it against yet — Phase 0 has no CSV import, no certificate
  template, no photo pipeline. Untested scaffolding built ahead of its
  first real caller tends to guess the wrong shape (job payload fields,
  retry/backoff policy, how a job's result surfaces back to the tenant)
  and becomes rework instead of a head start. The Object Storage module
  built in this pass (`internal/platform/objectstorage`,
  `internal/storage`) is exactly the piece Phase 1's
  workers *will* depend on (reading the uploaded CSV, writing the
  generated PDF), and that dependency is now solid.
- **What's already in place for this to slot into cleanly:** the
  `objects` table's `status` column (`pending`/`stored`) and
  `ActionObjectStored` audit action are patterns a job-status column and
  `job.completed` audit action can mirror directly; `RateLimiter`'s Redis
  client is the same client a queue would reuse; and `database.DB.
  WithTenantTx` is what a worker would wrap each job's DB work in, exactly
  like every HTTP handler does today.
