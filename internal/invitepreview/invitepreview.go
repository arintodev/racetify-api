// Package invitepreview answers "what is this invitation for?" for the
// /invite/{token} landing page, before the invitee has an account or is
// signed in. Workspace (staff) and event (crew) invitations share one link
// shape, so one unauthenticated endpoint asks each kind in turn.
//
// Possession of the token is the authorization (the same model as
// redeeming it), so the response is deliberately minimal: what the invite
// is for, its state, and a masked hint of the email it was issued to - not
// the address itself.
package invitepreview

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

const (
	KindWorkspace = "workspace"
	KindEvent     = "event"

	StatusPending  = "pending"
	StatusExpired  = "expired"
	StatusRevoked  = "revoked"
	StatusAccepted = "accepted"
)

// Preview is what a Source reports. Email is the full address; it never
// leaves this package unmasked.
type Preview struct {
	Kind string
	// Name is the workspace or event the invitation is for.
	Name string
	// EventSlug is set for event invitations, for the post-accept redirect.
	EventSlug string
	// Role is the workspace role (workspace invitations).
	Role string
	// Label and Capabilities describe an event assignment (event invitations).
	Label        string
	Capabilities []string
	Email        string
	Status       string
	ExpiresAt    time.Time
}

// Source looks an invitation up by its raw token, returning
// domain.ErrNotFound when the token is not one of its kind.
type Source interface {
	PreviewInvitation(ctx context.Context, rawToken string) (*Preview, error)
}

type previewDTO struct {
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	EventSlug    string    `json:"event_slug,omitempty"`
	Role         string    `json:"role,omitempty"`
	Label        string    `json:"label,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	EmailHint    string    `json:"email_hint"`
	Status       string    `json:"status"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// MaskEmail keeps enough of an address for its owner to recognise it
// ("r***@example.com") without revealing it to whoever holds the link.
func MaskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "***"
	}
	local, host := email[:at], email[at:]
	return string([]rune(local)[:1]) + "***" + host
}

// Status resolves the state shown to the invitee from a stored status and
// expiry: a pending invitation past its expiry is reported as expired.
func ResolveStatus(stored string, expiresAt, now time.Time) string {
	if stored == StatusPending && now.After(expiresAt) {
		return StatusExpired
	}
	return stored
}

// RegisterRoutes mounts GET /api/v1/invitations/preview?token=. It takes
// no session; rate limit it, since it is an unauthenticated lookup by
// secret.
func RegisterRoutes(mux *http.ServeMux, mw routing.Middlewares, sources ...Source) {
	mux.Handle("GET /api/v1/invitations/preview", routing.Chain(handler(sources), mw.SignupRateLimit))
}

func handler(sources []Source) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(r.URL.Query().Get("token"))
		if token == "" {
			respond.Error(w, http.StatusBadRequest, "invalid_request", "token is required.")
			return
		}
		for _, src := range sources {
			p, err := src.PreviewInvitation(r.Context(), token)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				respond.FromServiceError(w, err)
				return
			}
			respond.JSON(w, http.StatusOK, previewDTO{
				Kind: p.Kind, Name: p.Name, EventSlug: p.EventSlug, Role: p.Role,
				Label: p.Label, Capabilities: p.Capabilities,
				EmailHint: MaskEmail(p.Email), Status: p.Status, ExpiresAt: p.ExpiresAt,
			})
			return
		}
		respond.Error(w, http.StatusNotFound, "not_found", "This invitation link is not valid.")
	}
}
