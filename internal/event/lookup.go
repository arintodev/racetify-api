package event

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/httpapi/reqctx"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/platform/rbac"
)

// LookupPrefix is the path GET /events/lookup/{slug} lives under.
//
// It cannot be registered on the ServeMux with the other event routes:
// "/events/lookup/{slug}" and "/events/{id}/races" (and every other
// "/events/{id}/<name>") both match "/events/lookup/races" and neither is
// more specific, which Go's mux rejects at registration. So the router
// dispatches this prefix itself, before the mux (see LookupHandler).
const LookupPrefix = "/api/v1/events/lookup/"

// EventLookup is what an app needs to open an event by its URL slug.
type EventLookup struct {
	Event *Event
	Races []Race
	// Access says how the caller may see this event: as tenant staff (Role
	// set) or as external crew (Capabilities set).
	Access LookupAccess
}

type LookupAccess struct {
	Kind         string // "staff" | "crew"
	Role         string
	Capabilities []string
}

// LookupEvent resolves an event by slug for the caller. Tenant staff find it
// in their own tenant (the one their access token is scoped to); external
// crew find it among the events they are actively assigned to. Either way
// the caller must already have access to the event: a slug is not a way to
// discover events.
func (s *Service) LookupEvent(ctx context.Context, userID, claimTenantID, slug string) (*EventLookup, error) {
	// Staff path: the token's tenant, membership re-verified from the database.
	if claimTenantID != "" {
		var out *EventLookup
		err := s.db.WithTenantTx(ctx, claimTenantID, func(ctx context.Context) error {
			role, active, err := s.members.ActiveRole(ctx, claimTenantID, userID)
			if err != nil {
				return err
			}
			if !active || !rbac.MemberRole(role).IsAtLeast(rbac.RoleStaff) {
				return domain.ErrNotFound
			}
			ev, err := s.repo.GetEventBySlug(ctx, claimTenantID, slug)
			if err != nil {
				return err
			}
			races, err := s.repo.ListRaces(ctx, claimTenantID, ev.ID)
			if err != nil {
				return err
			}
			out = &EventLookup{Event: ev, Races: races, Access: LookupAccess{Kind: "staff", Role: role}}
			return nil
		})
		if err == nil {
			return out, nil
		}
		if err != domain.ErrNotFound {
			return nil, err
		}
		// Not staff-visible in that tenant: fall through to crew assignments.
	}

	// Crew path: an active assignment to an event with this slug.
	assignments, err := s.repo.MyAssignments(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range assignments {
		a := &assignments[i]
		if a.EventSlug != slug || !a.IsActive(now) {
			continue
		}
		var out *EventLookup
		err := s.db.WithTenantTx(ctx, a.TenantID, func(ctx context.Context) error {
			ev, err := s.repo.GetEventByID(ctx, a.TenantID, a.EventID)
			if err != nil {
				return err
			}
			races, err := s.repo.ListRaces(ctx, a.TenantID, a.EventID)
			if err != nil {
				return err
			}
			out = &EventLookup{Event: ev, Races: races, Access: LookupAccess{Kind: "crew", Capabilities: a.Capabilities}}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	return nil, domain.ErrNotFound
}

type lookupAccessDTO struct {
	Kind         string   `json:"kind"`
	Role         string   `json:"role,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type lookupDTO struct {
	EventDTO
	Races  []RaceDTO       `json:"races"`
	Access lookupAccessDTO `json:"access"`
}

// LookupHandler returns the path prefix and the handler for GET
// /api/v1/events/lookup/{slug}, which the router mounts ahead of the mux
// (see LookupPrefix for why).
func LookupHandler(mw routing.Middlewares, svc *Service) (prefix string, h http.Handler) {
	handle := func(w http.ResponseWriter, r *http.Request) {
		slug := strings.Trim(strings.TrimPrefix(r.URL.Path, LookupPrefix), "/")
		if slug == "" || strings.Contains(slug, "/") {
			respond.Error(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
			return
		}
		userID, _ := reqctx.UserID(r.Context())
		claimTenantID, _ := reqctx.TenantID(r.Context())

		res, err := svc.LookupEvent(r.Context(), userID, claimTenantID, slug)
		if err != nil {
			respond.FromServiceError(w, err)
			return
		}
		races := make([]RaceDTO, len(res.Races))
		for i := range res.Races {
			races[i] = raceResponse(&res.Races[i])
		}
		respond.JSON(w, http.StatusOK, lookupDTO{
			EventDTO: eventResponse(res.Event),
			Races:    races,
			Access:   lookupAccessDTO{Kind: res.Access.Kind, Role: res.Access.Role, Capabilities: res.Access.Capabilities},
		})
	}
	return LookupPrefix, routing.Chain(handle, mw.RequireUserAuth)
}
