# Racetify API — Phase 0 Bounded-Context Refactor Plan

> Planning document, no code changed yet. Extends the package-per-bounded-
> context restructure already agreed for Phase 1
> (`docs/phase1-api-plan.md` §9) back onto Phase 0, so both phases end up
> in one consistent shape instead of Phase 0 staying layered
> (`domain/repository/service/httpapi`) while Phase 1 alone is
> bounded-context. Grounded in the actual current Phase 0 source (read in
> full before writing this), not a generic template.

## 1. Why now, and what "bounded context" means here

Phase 1's plan already declared the target shape and the reason for it:
*"nothing outside a module shares its invariants (e.g. `bib_number` unique
within an Event is `participant`'s alone to enforce)"*. Phase 0 has the
same kind of invariants today, just hidden inside the layered split:
`MemberRole`'s owner>admin>staff ordering is `tenant`'s invariant,
`bib_number`-style uniqueness elsewhere is `oauth_clients.client_id`'s
invariant, etc. Doing this conversion **before** any Phase 1 code lands
means Phase 1's modules are built directly against the final shape, and
the `internal/httpapi/routing` extraction (§8 below) only has to happen
once, not once per phase.

## 2. Target bounded contexts

| Package | Owns (entities) | Owns (tables) | Current files moving in |
|---|---|---|---|
| `internal/auth` | `User`, `RefreshToken`, `EmailToken`, `SocialAccount` | `users`, `refresh_tokens`, `email_tokens`, `social_accounts` | `domain/user.go`, `domain/refreshtoken.go`, `domain/social_account.go`, `repository/user_repo.go`, `repository/refreshtoken_repo.go` (also defines `EmailTokenRepository` — confirmed by grep, not a separate file), `repository/social_account_repo.go`, `service/auth_service.go`, `service/google_oauth.go`, `httpapi/handlers/auth_handler.go`, `httpapi/routes_auth.go` |
| `internal/tenant` | `Tenant`, `TenantMember`, `MemberRole`, `Invitation` | `tenants`, `tenant_members`, `invitations` | `domain/tenant.go`, `domain/invitation.go`, `repository/tenant_repo.go`, `repository/membership_repo.go`, `repository/invitation_repo.go`, `service/tenant_service.go`, `httpapi/handlers/tenant_handler.go`, `httpapi/routes_tenant.go`, **plus** `httpapi/handlers/resource_handler.go` + `httpapi/routes_resource.go` (see §6) |
| `internal/oauthclient` | `OAuthClient`, `Scope` | `oauth_clients` | `domain/oauthclient.go`, `domain/scope.go`, `repository/oauthclient_repo.go`, `service/oauthclient_service.go`, `httpapi/handlers/oauthclient_handler.go`, `httpapi/routes_oauth.go` |
| `internal/storage` | `Object` | `objects` | `domain/object.go`, `repository/object_repo.go`, `service/storage_service.go`, `httpapi/handlers/storage_handler.go`, `httpapi/routes_storage.go` |

Note the name collision risk that **isn't** one: `internal/storage` (this
bounded context: the `objects` table, upload tickets, `StorageService`)
is distinct from `internal/platform/objectstorage` (the `Driver`
interface, `local_driver.go`, `r2_driver.go` — how bytes actually get
written). That split already exists today (`service/storage_service.go`
vs `platform/objectstorage/*`); this refactor doesn't change it, just
moves the former out of the layered `service`/`repository` packages.

Likewise `internal/tenant` (this bounded context: the `Tenant` business
entity, membership, invitations) is distinct from "multi-tenancy" the
cross-cutting mechanism (`tenant_id` columns, RLS, `WithTenantTx`), which
stays exactly where it is in `internal/platform/database` and is used
*by* every bounded context, not owned by `internal/tenant`.

## 3. What stays shared, and why

| Package | Contents | Why it doesn't become a bounded context |
|---|---|---|
| `internal/domain` | Only `errors.go` after this refactor (`ErrNotFound`, `ErrConflict`, etc.) | Every bounded context's repository translates SQL errors into these; `httpapi/respond` maps them to HTTP status/code. Zero owning logic, pure shared vocabulary — same role `io.EOF` plays in the stdlib. Doc comment gets updated to say so explicitly instead of its current "holds the plain data types shared across services" description, which is going away. |
| `internal/audit` (**new**, promoted) | `domain/audit.go` (`AuditLog`, action consts) + `repository/audit_repo.go` merged together | Called from every bounded context's service to append a log entry, but owns no invariant any single context enforces — it's a horizontal concern, same shape as Phase 1's planned `internal/jobqueue` ("top-level, app-orchestration, not raw infra"). Left split across a now-empty `domain`/`repository` remnant would just be two fossil packages; merging into one small top-level package is honest about what's left. |
| `internal/platform/pagination` (**new**, moved) | `repository/pagination.go`'s `PageParams`/`NormalizeLimit`/`DefaultPageLimit`/cursor codec, plus its two test files | Pure cursor-encoding, zero HTTP or domain awareness, reused by every bounded context's `repository.go`. Matches `platform`'s existing shape (`database`, `rediscli`, `logger`, `objectstorage` — infra with no business knowledge). |
| `internal/httpapi/respond` (existing, gains one file) | Adds `pagination.go`: `pageParamsFromRequest` + `ListResponseDTO[T]`, moved from `httpapi/handlers/pagination.go` | These *are* HTTP-layer (parse `r.URL.Query()`, shape a JSON envelope) — `respond` already exists for "how every handler shapes a response," so this is its natural home, no new package needed. |
| `internal/platform/ratelimit` (**new**, moved) | `service/ratelimit.go`'s `RateLimiter`/`Allow`, verified to import only `rediscli` | Zero domain coupling (a Redis fixed-window counter keyed by a plain string) — used by both `auth` (login) and `oauthclient` (token grant), so it can't live inside either. |
| `internal/security`, `internal/mailer`, `internal/platform/*` | Unchanged | Already correctly-scoped leaf infra — `security.TokenManager` doesn't know what a "user" is, `mailer.Mailer` doesn't know what an "invitation" is. Confirmed by re-reading `middleware/auth.go`: it already imports `internal/security` with zero domain coupling, which is the pattern everything above is being brought in line with. |
| `internal/httpapi/{reqctx,respond}` | Unchanged (respond gains one file, above) | Already decoupled — `reqctx.WithMemberRole` stores the role as a plain `string`, never the `MemberRole` type, so it needs no import from `tenant`. |

## 4. Two real coupling snags this surfaces, and how they're resolved

Reading the actual middleware source (not assuming) turned up two spots
where `internal/httpapi/middleware` currently imports business types
directly. Both need a decision, because once those types move into
bounded-context packages, `middleware` importing them the same way it
does today would create the same kind of import-cycle risk the
`chain`/`mws` extraction was already built to avoid for Phase 1 — a
bounded-context package needs `middleware`'s constructors to guard its
own routes, so `middleware` can't depend back on that package.

**4.1 `RequireRole(min domain.MemberRole)` → moves into `tenant`.**
Role-hierarchy comparison (`owner > admin > staff`) is `tenant`'s own
invariant, exactly like `bib_number` uniqueness is `participant`'s in the
Phase 1 plan. Fix: the function moves to `internal/tenant` as
`tenant.RequireRole(min tenant.MemberRole) func(http.Handler)
http.Handler`. `RequireSuperAdmin` stays in `middleware` unchanged — it
only reads a bool off the JWT claims (`reqctx.IsSuperAdmin`), no
`MemberRole` involved.

**4.2 `RequireTenantForUser(db, members *repository.MembershipRepository)`
→ narrowed to an interface `middleware` itself declares.** This is the
one real inconsistency in an otherwise well-decoupled middleware package:
`RequireUserAuth` already avoids importing `service` by taking a plain
`isRevoked func(ctx, jti) bool` callback instead — its own doc comment
says why ("to avoid this package importing service - service stays a
strict 'no net/http' layer"). `RequireTenantForUser` doesn't follow that
pattern today; this refactor brings it in line with it rather than
introducing a new `middleware → tenant` package dependency:

```go
// internal/httpapi/middleware/tenant.go — narrowed, no tenant import
type MembershipChecker interface {
    ActiveRole(ctx context.Context, tenantID, userID string) (role string, active bool, err error)
}
func RequireTenantForUser(db *database.DB, members MembershipChecker) func(http.Handler) http.Handler
```

`tenant.Repository` implements `ActiveRole` (a thin wrapper around
today's `members.Get` + the `MemberStatusActive` check, both of which
move into `tenant` anyway) and satisfies this interface structurally —
`middleware` never imports `tenant`, and `err` is reserved for genuine
infra failures rather than forcing a `domain.ErrNotFound` identity
comparison inside `middleware`.

With both fixes, `internal/httpapi/middleware` ends this refactor
importing **no** bounded-context package — same guarantee `routing` was
already designed to give the route-registration side.

## 5. OAuth scopes move into `oauthclient`

`domain/scope.go`'s constants and `IsValidScope` are read directly by
`oauthclient_service.go` when validating a client-creation request (a
genuine `oauthclient` invariant: "a client can only be granted scopes
that exist") — unlike audit/errors, this has an owning context. Scopes
move to `oauthclient.ScopeEventsRead` etc. Every other bounded context
that gates an M2M route by scope (today: `tenant`'s resource-summary
M2M variant; later: Phase 1's `event`/`participant`) imports
`oauthclient` downward for the string constant only — a harmless
one-directional edge, since `oauthclient` never imports any of them back.
`middleware.RequireScope(scope string)` itself needs no change — it
already takes a plain string, confirmed by reading it.

## 6. `resource_handler.go` folds into `tenant`

`ResourceHandler`'s own doc comment says it "exists purely to demonstrate
Phase 0 deliverable #2/#3" — a proof endpoint, not product surface.
Its `Summary` method is tenant data (`TenantService.GetTenantSummary`,
which already does a raw SQL `count(*) FROM oauth_clients` rather than
importing an oauth-client repository — confirmed by reading it, so this
move introduces no `tenant → oauthclient` package dependency either).
Rather than inventing a fifth bounded context for one demo endpoint, it
becomes `tenant.Handler.Summary` plus both its route variants
(`GET /api/v1/tenant/summary` user-session, `GET /api/v1/m2m/tenant/summary`
M2M) inside `tenant/routes.go`. Low-stakes either way — flagging the
choice, not blocking on it.

## 7. New `internal/httpapi/routing` package (pulled forward from Phase 1)

Same fix already designed in `docs/phase1-api-plan.md` §9 Problem 1,
just built now instead of later: `chain()` and the `mws` struct move out
of `router.go` into a neutral leaf package both sides import downward:

```go
// internal/httpapi/routing/routing.go
type Middlewares struct {
    RequireUserAuth, RequireM2MAuth, RequireTenantForUser,
    RequireAdminRole, RequireAnyRole, RequireSuperAdmin,
    LoginRateLimit, TokenRateLimit func(http.Handler) http.Handler
}
func Chain(h http.HandlerFunc, mws ...func(http.Handler) http.Handler) http.Handler
```

`RequireAdminRole`/`RequireAnyRole` are now built in `router.go` by
calling `tenant.RequireRole(tenant.RoleAdmin)` / `tenant.RequireRole(tenant.RoleStaff)`
instead of `middleware.RequireRole(domain.RoleAdmin)` — the only call-site
change `router.go` needs from §4.1. Each bounded context gets a
`routes.go` exposing `RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, svc *Service)`,
importing only `routing` (never `middleware` directly), matching the
pattern already agreed for Phase 1's five modules.

## 8. Composition root (`internal/app.Build`) — still exactly one

No change to the rule itself, just to what's being constructed.
`app.Build` swaps each `repository.NewXRepository`/`service.NewXService`
call for the matching bounded-context constructor
(`auth.NewRepository`, `auth.NewService`, `tenant.NewRepository`,
`tenant.NewService`, `oauthclient.NewRepository`, `oauthclient.NewService`,
`storage.NewRepository`, `storage.NewService`, `audit.NewRepository`),
and `httpapi.Deps` field types change to match (`Tenants *tenant.Service`,
`OAuthClient *oauthclient.Service`, `Storage *storage.Service`, ...).
`Deps.Users`/`Deps.Memberships` — read directly by `router.go` and
`auth_handler.go` today — become `Deps.Auth.Repo`-style access or get
dropped in favor of methods on the owning service, decided during
execution once the exact call sites are in front of us rather than
guessed here. `test/integration/*` keeps calling `app.Build` unchanged,
so its black-box HTTP assertions don't need touching — only the unit
tests that import moved packages directly do (§10).

## 9. Required follow-up: sync `docs/phase1-api-plan.md`

That document currently says, in two places, things this refactor makes
stale:

1. §2: *"`gallery`/`generator` ... depend on `internal/service.StorageService`"*
   → becomes `storage.Service`.
2. §9: *"Phase 0 keeps that layered shape untouched — it's genuinely
   shared platform, not a business domain"* → no longer true; Phase 0's
   `auth`/`tenant`/`oauthclient`/`storage` sit beside Phase 1's five
   modules as peers, and the `internal/domain`/`repository`/`service`
   layered packages this line refers to won't exist anymore.

Not editing `phase1-api-plan.md` yet since this plan itself isn't
executed — noting it here so it isn't forgotten once it is.

## 10. Full file move table

| Today | Becomes |
|---|---|
| `domain/user.go`, `refreshtoken.go`, `social_account.go` | `auth/user.go`, `refreshtoken.go`, `social_account.go` |
| `repository/user_repo.go`, `refreshtoken_repo.go` (incl. `EmailTokenRepository`), `social_account_repo.go` | `auth/repository.go` (one file, entities split as above) |
| `service/auth_service.go`, `google_oauth.go` | `auth/service.go`, `google_oauth.go` |
| `httpapi/handlers/auth_handler.go` + relevant slice of `handlers/dto.go` (`UserDTO`, `SessionDTO`) | `auth/handler.go`, `auth/dto.go` |
| `httpapi/routes_auth.go` | `auth/routes.go` |
| `domain/tenant.go`, `invitation.go` | `tenant/tenant.go`, `invitation.go` |
| `repository/tenant_repo.go`, `membership_repo.go`, `invitation_repo.go` | `tenant/repository.go` |
| `service/tenant_service.go` | `tenant/service.go` |
| `httpapi/handlers/tenant_handler.go`, `resource_handler.go` + relevant slice of `dto.go` (`TenantDTO`, `TenantWithRoleDTO`, `MemberDTO`, `InvitationDTO`, `InvitationCreatedDTO`) | `tenant/handler.go`, `tenant/dto.go` |
| `httpapi/routes_tenant.go`, `routes_resource.go` | `tenant/routes.go` |
| `domain/oauthclient.go`, `scope.go` | `oauthclient/oauthclient.go`, `scope.go` |
| `repository/oauthclient_repo.go` | `oauthclient/repository.go` |
| `service/oauthclient_service.go` | `oauthclient/service.go` |
| `httpapi/handlers/oauthclient_handler.go` + slice of `dto.go` (`OAuthClientDTO`, `OAuthClientCreatedDTO`) | `oauthclient/handler.go`, `oauthclient/dto.go` |
| `httpapi/routes_oauth.go` | `oauthclient/routes.go` |
| `domain/object.go` | `storage/object.go` |
| `repository/object_repo.go` | `storage/repository.go` |
| `service/storage_service.go` | `storage/service.go` |
| `httpapi/handlers/storage_handler.go` + slice of `dto.go` (`ObjectDTO`, `UploadTicketDTO`, `DownloadTicketDTO`) | `storage/handler.go`, `storage/dto.go` |
| `httpapi/routes_storage.go` | `storage/routes.go` |
| `domain/audit.go` + `repository/audit_repo.go` | `audit/audit.go`, `audit/repository.go` |
| `domain/errors.go` | `domain/errors.go` (unchanged, doc comment updated) |
| `repository/pagination.go` + its 2 test files | `platform/pagination/pagination.go` + tests |
| `httpapi/handlers/pagination.go` | `httpapi/respond/pagination.go` |
| `service/ratelimit.go` | `platform/ratelimit/ratelimit.go` |
| `httpapi/middleware/rbac.go`'s `RequireRole` | `tenant/authz.go` |
| `httpapi/middleware/tenant.go`'s `RequireTenantForUser` | stays in `middleware/tenant.go`, signature narrowed (§4.2) |
| `httpapi/router.go`'s `chain`/`mws` | `httpapi/routing/routing.go` (`Chain`/`Middlewares`, exported) |
| `httpapi/handlers/health_handler.go`, `routes_health.go` | unchanged — no bounded-context owner, stays technical/ops |
| `security/*`, `mailer/*`, `platform/{database,logger,objectstorage,rediscli}` | unchanged |
| `repository/rls_isolation_test.go` | stays put or moves to `test/integration/` — it exercises cross-tenant isolation across *multiple* tables/contexts at once by design, so forcing it into one bounded context's package would be artificial. Decide at execution time. |

## 11. Target folder structure

```
racetify-api/internal/
├── auth/            user.go refreshtoken.go social_account.go
│                     repository.go  service.go  google_oauth.go
│                     handler.go  dto.go  routes.go
├── tenant/           tenant.go invitation.go
│                     repository.go  service.go  authz.go   (RequireRole, tenant's own invariant)
│                     handler.go  dto.go  routes.go          (incl. the old resource_handler summary)
├── oauthclient/      oauthclient.go scope.go
│                     repository.go  service.go
│                     handler.go  dto.go  routes.go
├── storage/          object.go
│                     repository.go  service.go
│                     handler.go  dto.go  routes.go
├── audit/            audit.go  repository.go          NEW — promoted, cross-cutting log
├── domain/           errors.go                        SLIMMED — sentinel errors only
├── security/         jwt.go password.go secret.go uuid.go     unchanged
├── mailer/           mailer.go                                unchanged
├── platform/
│   ├── database/ logger/ objectstorage/ rediscli/              unchanged
│   ├── pagination/  pagination.go (+ tests)             NEW — moved from repository/
│   └── ratelimit/   ratelimit.go                        NEW — moved from service/
├── httpapi/
│   ├── router.go                     shrinks to: build routing.Middlewares, call each RegisterRoutes
│   ├── routes_health.go              unchanged
│   ├── handlers/health_handler.go    unchanged
│   ├── middleware/                   auth.go tenant.go(narrowed) ratelimit.go observability.go
│   │                                 — rbac.go's RequireRole removed (→ tenant/authz.go), RequireSuperAdmin stays
│   ├── respond/                      respond.go + pagination.go (moved from handlers/)
│   ├── reqctx/                       unchanged
│   └── routing/                      routing.go   NEW — Chain/Middlewares, shared by Phase 0 AND Phase 1
└── app/app.go                        composition root — constructs the 5 bounded contexts' Service/Repository
```

## 12. Suggested execution order

One bounded context at a time, tests green before moving to the next —
this is already-shipped, already-tested code, so the goal is zero
behavior change, not a big-bang rewrite:

1. `internal/httpapi/routing` extraction (§7) + `internal/domain/errors.go` doc-comment update — mechanical, touches every route file's imports but no logic. Build/test after.
2. `internal/audit` promotion (§3) — small, isolated, everything else already depends on it through a stable interface shape.
3. `internal/platform/pagination` + `internal/httpapi/respond/pagination.go` moves (§3) — mechanical, no logic change.
4. `internal/platform/ratelimit` move (§3) — mechanical.
5. `internal/oauthclient` (smallest bounded context, fewest cross-references) — proves the entity+repo+service+handler+routes pattern end to end.
6. `internal/storage` — next-smallest, but confirm §9 of `phase1-api-plan.md` gets its `storage.Service` rename queued.
7. `internal/tenant` — includes the two coupling fixes (§4.1, §4.2) and the `resource_handler.go` fold-in (§6); do this only after `oauthclient`/`storage` prove the pattern, since it's the one with real snags to work through.
8. `internal/auth` — last, since `RequireUserAuth`'s `isRevoked` callback already decouples it from everything else, making it the lowest-risk move but also the one with the most files.
9. Full `go build ./... && go vet ./... && make test` pass, then the `docs/phase1-api-plan.md` sync (§9 above).

## 13. Risk notes

- This touches every file in Phase 0 except `security`/`mailer`/`platform/{database,logger,objectstorage,rediscli}` — the risk is entirely mechanical (import paths, package names), not behavioral, if each step in §12 is verified independently rather than done as one large diff.
- `rls_isolation_test.go`'s placement (§10) is the one open call worth a decision before execution starts, since it's the only file that doesn't cleanly belong to one bounded context by construction.
- No database migration is involved anywhere in this plan — it is a Go package reorganization only.
