package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/bibprint"
	"github.com/racetify/racetify-api/internal/certificate"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/event"
	"github.com/racetify/racetify-api/internal/fontlib"
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
		Events: &event.Service{}, Participants: &participant.Service{}, Templates: &generator.Service{}, Certificates: &certificate.Service{}, BibPrint: &bibprint.Service{}, Fonts: &fontlib.Service{}, JobQueue: &jobqueue.Queue{},
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
		{"POST", "/api/v1/events/abc/bib-generation"},
		{"GET", "/api/v1/fonts"},
		{"GET", "/api/v1/fonts/files/abc/bold"},
		{"GET", "/api/v1/fonts/bundled/liberation-sans/regular"},
		{"GET", "/api/v1/admin/fonts"},
		{"POST", "/api/v1/admin/fonts"},
		{"GET", "/api/v1/admin/fonts/demand"},
		{"PATCH", "/api/v1/admin/fonts/abc"},
		{"DELETE", "/api/v1/admin/fonts/abc"},
		{"PUT", "/api/v1/admin/fonts/abc/files/bold"},
		{"DELETE", "/api/v1/admin/fonts/abc/files/bold"},
		{"GET", "/api/v1/events/abc/bib-generation/job1/download"},
		{"GET", "/api/v1/events/abc/certificates"},
		{"PUT", "/api/v1/events/abc/certificates/def"},
		{"DELETE", "/api/v1/events/abc/certificates/def"},
		{"POST", "/api/v1/events/abc/certificates/publish"},
		{"POST", "/api/v1/events/abc/certificates/generate"},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}
