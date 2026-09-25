package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/event"
	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/oauthclient"
	"github.com/racetify/racetify-api/internal/participant"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
	"github.com/racetify/racetify-api/internal/tenant"
)

// TestRoutesRegister builds the full router with empty services. Go's
// ServeMux panics at registration when two patterns overlap without one
// being more specific (e.g. /events/lookup/{slug} vs /events/{id}/races), so
// a successful build proves the route table is coherent - something no
// database is needed to check.
func TestRoutesRegister(t *testing.T) {
	cfg := &config.Config{}
	router := NewRouter(Deps{
		Config: cfg, Logger: slog.Default(),
		Tokens: security.NewTokenManager("test-secret", "test"),
		Auth:   &auth.Service{}, Google: &auth.GoogleService{}, Tenants: &tenant.Service{},
		OAuthClient: &oauthclient.Service{}, Storage: &storage.Service{},
		Events: &event.Service{}, Participants: &participant.Service{}, Templates: &generator.Service{}, JobQueue: &jobqueue.Queue{},
	})

	// Unauthenticated requests must be turned away before any service is
	// touched (the services here are empty).
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/events/lookup/jakarta-marathon-2026"},
		{"GET", "/api/v1/events/lookup/races"}, // a slug that collides with a route name
		{"GET", "/api/v1/events/abc/participants"},
		{"GET", "/api/v1/events/abc/participants/summary"},
		{"GET", "/api/v1/events/abc/participants/export"},
		{"POST", "/api/v1/events/abc/participants/import"},
		{"GET", "/api/v1/events/abc/races"},
		{"GET", "/api/v1/events/abc/templates"},
		{"GET", "/api/v1/events/abc/templates/def"},
		{"POST", "/api/v1/events/abc/templates"},
		{"PATCH", "/api/v1/events/abc/templates/def"},
		{"DELETE", "/api/v1/events/abc/templates/def"},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}
