# Racetify API — Event-Scoped Crew/Volunteer Access (Planning)

> Planning document, written before any code for this feature exists.
> Extends `phase1-api-plan.md` rather than replacing it: Events, Races,
> Participants, Generator, Media Gallery and Jobs from that plan are
> assumed to already exist as designed there. Nothing in this document is
> implemented yet.

## 1. Problem

`racetify_prd_final.md` §4's Roles & Permissions Matrix names two
event-operational roles distinct from the tenant workspace roles already
built in Phase 0:

* **Event Manager/Staff:** "Akses terbatas untuk operasional event
  tertentu."
* **Crew/Field Operator:** "Akses berbasis scanner untuk operasional RPC
  atau validasi lapangan."

Today's RBAC (`internal/tenant`, `internal/platform/rbac`) has no notion
of "this event only." A `tenant_members` row carries one role
(`owner`/`admin`/`staff`) that applies to **every** event in the tenant —
`tenant.RequireRole(RoleStaff)` is the gate on essentially every Phase 1
event-scoped route (`phase1-api-plan.md` §4), with no per-event narrowing
at all.

The first draft of this design tried to fix that by adding a `crew`
tenant role plus an assignment table keyed off `tenant_members.id`. That
was wrong: it assumed crew are always part of the organizer's workspace
roster. In practice a lot of race-day crew are **not** — a freelance RPC
operator hired for one event, a volunteer who shows up once, a
photographer who works for several different organizers. Forcing them
through `tenant_members` would mean either inventing a workspace
membership for someone who isn't part of the workspace, or blocking
external crew entirely. Neither is right, so this plan keys assignment
off the person's Racetify identity (`users.id`) directly, with **no**
dependency on `tenant_members`.

## 2. Model

### 2.1 One identity, two independent relationships to a tenant

A person can relate to a tenant in either or both of two ways, and the
two are intentionally decoupled:

1. **Workspace membership** (`tenant_members`, unchanged) — "I'm part of
   this organizer's team," tenant-wide, `owner`/`admin`/`staff`.
2. **Event assignment** (new, this doc) — "I'm assigned to work this
   specific event," with a narrow, explicit capability set. Requires no
   workspace membership.

A `tenant_members` row and an `event_assignments` row for the same person
can both exist (an internal Staff additionally scoped down to 2 of the
tenant's 10 events) or only the second can exist (a purely external
crew/volunteer/photographer who never joins the workspace roster at all).
The two tables never reference each other — assignment always keys off
`users.id`, matching this codebase's existing Single Identity principle
(`racetify_prd_final.md` §2.2: one account usable across events, same
identity the Runner Portal is built around).

### 2.2 New entity: `EventAssignment`

```go
// Package event addition (event.go)

type AssignmentStatus string

const (
    AssignmentStatusActive  AssignmentStatus = "active"
    AssignmentStatusRevoked AssignmentStatus = "revoked"
)

// EventAssignment grants a user_id narrow, capability-scoped access to
// exactly one Event, independent of whether that user is a tenant_member.
// This is what makes "external RPC crew for one event" and "internal
// Staff narrowed to 2 of 10 events" the same mechanism (see
// docs/event-crew-access-plan.md §2.1).
type EventAssignment struct {
    ID           string
    TenantID     string
    EventID      string
    UserID       string
    // Label is display-only ("crew", "volunteer", "photographer",
    // "timing") - Capabilities is what authorization actually checks.
    Label        string
    Capabilities []string
    Status       AssignmentStatus
    AssignedBy   string
    ExpiresAt    *time.Time
    CreatedAt    time.Time
    UpdatedAt    time.Time
}
```

### 2.3 Migration `0011_event_assignments.up.sql`

Following the existing pattern — RLS inline in the same file that creates
the table, plain `TEXT` status (`0001_core.up.sql`'s documented reasoning),
`set_updated_at` trigger.

```sql
CREATE TABLE event_assignments (
    id           UUID PRIMARY KEY,
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_id     UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label        TEXT NOT NULL DEFAULT 'crew',
    capabilities TEXT[] NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    assigned_by  UUID NOT NULL REFERENCES users(id),
    expires_at   TIMESTAMPTZ NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One assignment row per (event, person) - re-assigning with new
    -- capabilities is an UPDATE, not a second row.
    CONSTRAINT event_assignments_uk UNIQUE (event_id, user_id)
);

CREATE TRIGGER event_assignments_set_updated_at
    BEFORE UPDATE ON event_assignments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX event_assignments_tenant_id_idx ON event_assignments(tenant_id);
CREATE INDEX event_assignments_event_id_idx ON event_assignments(event_id);
-- What RequireEventAccess (§4) actually queries: "does this user have an
-- active assignment on this event."
CREATE INDEX event_assignments_user_event_idx ON event_assignments(user_id, event_id) WHERE status = 'active';

ALTER TABLE event_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON event_assignments
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
```

`tenant_id` is denormalized from `event_id` (same call as `races.tenant_id`
in `0006_events_races.up.sql`) purely so RLS can filter this table
directly without a join.

### 2.4 Capability vocabulary (initial set)

Free-form `TEXT[]`, not an enum column — same reasoning as every other
status column in this schema (`0001_core.up.sql`'s doc comment: the legal
set is enforced in Go, not the DB), and capabilities will grow as Phase 1
modules land. Starting set, one per module that plausibly needs
non-Staff, event-scoped access:

| Capability | Grants |
|---|---|
| `participants:read` | Look up/search participants within the event (BIB lookup for RPC counter staff) |
| `rpc:checkin` | Mark a participant's race pack as collected (not built yet — no RPC/check-in table exists; this is the capability that table's endpoint will check) |
| `results:write` | Manual result correction for this event only (narrower than the tenant-wide `timing:write` M2M scope already in `internal/domain/scope.go`) |
| `gallery:upload` | Upload photos to this event's albums (Photographer role) |

`internal/event.Service` owns the legal set (`ValidCapabilities()`), same
shape as `EventStatus`'s const block.

## 3. Identity & assignment flow

Reuses the shape of `internal/tenant/invitation.go`'s invite-by-email flow,
but the destination row is `event_assignments`, never `tenant_members`,
and acceptance never grants workspace membership:

1. Owner/Admin/Staff of the tenant calls
   `POST /api/v1/events/{id}/assignments` with
   `{email, label, capabilities, expires_at?}`.
2. If `email` matches an existing `users` row, the assignment can be
   created directly (`status=active`). If not, an `EventInvitation`
   token (mirroring `Invitation` in shape, but pointing at `event_id` +
   `capabilities` instead of `tenant_id` + `role`) is issued; accepting it
   runs the normal Single Identity signup if the invitee has no account
   yet, then inserts the `event_assignments` row.
3. Either way, the resulting person has **zero** rows in `tenant_members`
   unless they independently are one. They cannot list, switch into, or
   see anything about the tenant workspace itself — only the one (or few)
   events they were assigned to (see §5).

Revocation: `DELETE /api/v1/events/{id}/assignments/{assignmentId}` sets
`status=revoked` (soft delete, matching `TenantMember`'s
`removed`/`InvitationStatusRevoked` pattern elsewhere in this codebase) —
never a hard delete, since audit needs the historical record of who had
access during the event.

## 4. Authorization: resolving tenant context without a tenant session

This is the part that doesn't already exist in any form. Every
user-session route today reaches tenant context exactly one way —
`middleware.RequireTenantForUser` — which **requires** an active
`tenant_members` row, because the `tid` claim it trusts is only ever
minted by `POST /auth/switch-tenant`, which itself only lets a caller
select a tenant they're a member of. An externally-assigned crew member
has no membership row, so this path always 403s them ("You are not an
active member of this tenant") — correctly, for what it's designed to
check, but it's the wrong check for this caller.

A pure per-event caller doesn't need to "select a tenant" at all — the
`event_id` already in the URL pins the tenant unambiguously. This is
exactly the bootstrap problem `phase1-api-plan.md` §5 already solved once,
for a different caller with no session at all: `ResolvePublicEvent`
resolves `tenant_id` from `eventSlug` in the URL (a narrow, read-only
lookup) and injects it into context "the same shape `RequireTenantForM2M`
does," so every downstream repository call still goes through
`WithTenantTx`/RLS unchanged, with no membership check because there's no
principal to check membership for. `RequireEventAccess` below is the same
bootstrap trick, plus a membership check — because unlike the public
portal, there *is* a principal here, just not a tenant-member one.

```go
// internal/event/access.go (new)

// RequireEventAccess gates an event-scoped route (event_id in the URL,
// pattern {id}) to either an existing tenant session with role >= Staff
// for that event's tenant (the internal-staff path, unchanged from
// today), or an active EventAssignment carrying the given capability
// (the external crew/volunteer path, new). It must run after
// RequireUserAuth only - unlike RequireTenantForUser it does NOT require
// RequireTenantForUser/a tid claim to already be resolved, since deriving
// tenant_id from the event_id path param is exactly its job.
func RequireEventAccess(capability string, events EventTenantResolver, assignments AssignmentChecker) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            userID, ok := reqctx.UserID(r.Context())
            if !ok {
                respond.Error(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
                return
            }
            eventID := r.PathValue("id")

            tenantID, err := events.TenantIDForEvent(r.Context(), eventID)
            if err != nil {
                respond.Error(w, http.StatusNotFound, "not_found", "Event not found.")
                return
            }

            // Path A: caller already has a tenant session for this
            // event's tenant with role >= Staff - today's behavior,
            // unchanged. A session for a *different* tenant, or no
            // session at all, just falls through to Path B rather than
            // erroring here.
            if sessionTenantID, ok := reqctx.TenantID(r.Context()); ok && sessionTenantID == tenantID {
                if role, ok := reqctx.MemberRole(r.Context()); ok && tenant.MemberRole(role).IsAtLeast(tenant.RoleStaff) {
                    next.ServeHTTP(w, r)
                    return
                }
            }

            // Path B: no qualifying tenant session - fall back to a
            // direct, capability-scoped EventAssignment lookup. This is
            // the only path an externally-assigned crew/volunteer ever
            // takes; they never acquire a tid claim at all.
            granted, err := assignments.HasCapability(r.Context(), tenantID, eventID, userID, capability)
            if err != nil {
                respond.Error(w, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
                return
            }
            if !granted {
                respond.Error(w, http.StatusForbidden, "forbidden", "You are not assigned to this event.")
                return
            }

            ctx := reqctx.WithTenantID(r.Context(), tenantID)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

Route wiring (once an event-scoped module actually needs this — e.g. a
future RPC check-in endpoint):

```go
mux.Handle("POST /api/v1/events/{id}/checkin/scan", routing.Chain(
    h.Scan,
    event.RequireEventAccess("rpc:checkin", eventRepo, assignmentRepo),
    mw.RequireUserAuth,
))
```

Note there is deliberately **no** `mw.RequireTenantForUser` in that chain
— `RequireEventAccess` subsumes it for this route, handling both the
internal-staff case and the external-assignment case itself. Tenant-wide
administrative routes (event CRUD, tenant staff management, billing, API
keys) are untouched: they keep `RequireTenantForUser` +
`tenant.RequireRole` exactly as `phase1-api-plan.md` §4 already specifies,
so nothing already built regresses.

## 5. API surface

| Method & Path | Role | Purpose |
|---|---|---|
| `POST /api/v1/events/{id}/assignments` | Staff (tenant, that event's own team) | Assign by email: `{email, label, capabilities, expires_at?}` |
| `GET /api/v1/events/{id}/assignments` | Staff | List who's assigned to this event |
| `PATCH /api/v1/events/{id}/assignments/{aid}` | Staff | Change `capabilities`/`expires_at` |
| `DELETE /api/v1/events/{id}/assignments/{aid}` | Staff | Revoke (soft) |
| `GET /api/v1/me/event-assignments` | Any authenticated user | The caller's own view: every event they're assigned to, across tenants, with their capabilities on each — this is the externally-assigned crew's equivalent of "select a tenant," since they have no workspace to switch into |

## 6. Explicitly out of scope for this pass

* **Anonymous, account-less day-of volunteers** (a shared PIN/QR per
  check-in station, no `users` row at all). Real for race-day operations
  but a genuinely separate auth mechanism (not a JWT session at all), and
  everything in §4 already works for any volunteer willing to hold a
  Racetify account, which covers recurring crew. Worth a follow-up doc if
  and when the RPC module is actually scoped, not before.
* **The RPC/check-in table and endpoints themselves** — don't exist yet
  (Phase 1 migrations stop at `0010_jobs`); `rpc:checkin` above is named
  now only so the capability vocabulary doesn't need a breaking rename
  later.
* **Granular capability enum with DB-level `CHECK`** — starting with
  free-form `TEXT[]` validated in Go, consistent with every status column
  in this schema; revisit only if cross-capability queries need it.
