// Package httpapi assembles the HTTP server: it builds the shared
// middleware chain (internal/httpapi/middleware, internal/httpapi/routing.
// Middlewares) and hands it to each bounded context's own self-registering
// RegisterRoutes(mux, mw, ...) - internal/auth, internal/tenant, internal/
// oauthclient, internal/storage - using the standard library's Go 1.22+
// method+pattern ServeMux (no router dependency was needed). Only the
// health check has no bounded context of its own, so it keeps the older
// routes_health.go + handlers.HealthHandler shape built directly in
// NewRouter below.
package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/event"
	"github.com/racetify/racetify-api/internal/gallery"
	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/httpapi/handlers"
	"github.com/racetify/racetify-api/internal/httpapi/middleware"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
	"github.com/racetify/racetify-api/internal/invitepreview"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/oauthclient"
	"github.com/racetify/racetify-api/internal/participant"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/originpolicy"
	"github.com/racetify/racetify-api/internal/platform/ratelimit"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
	"github.com/racetify/racetify-api/internal/tenant"
)

// Deps is every dependency the router needs. cmd/api/main.go builds one of
// these after wiring the platform layer (db/redis) and service layer.
type Deps struct {
	Config  *config.Config
	Logger  *slog.Logger
	DB      *database.DB
	AdminDB *database.DB
	Redis   *rediscli.Client
	Tokens  *security.TokenManager

	Memberships *tenant.Repository

	Auth         *auth.Service
	Google       *auth.GoogleService
	Tenants      *tenant.Service
	OAuthClient  *oauthclient.Service
	RateLimiter  *ratelimit.Limiter
	Storage      *storage.Service
	ObjectStore  objectstorage.Driver
	Events       *event.Service
	Participants *participant.Service
	Templates    *generator.Service
	Gallery      *gallery.Service
	JobQueue     *jobqueue.Queue
}

func NewRouter(d Deps) http.Handler {
	healthH := handlers.NewHealthHandler(d.DB, d.Redis)

	mw := routing.Middlewares{
		RequireUserAuth:      middleware.RequireUserAuth(d.Tokens, d.Auth.IsAccessTokenRevoked),
		RequireM2MAuth:       middleware.RequireM2MAuth(d.Tokens),
		RequireTenantForUser: middleware.RequireTenantForUser(d.DB, d.Memberships),
		RequireAdminRole:     tenant.RequireRole(tenant.RoleAdmin),
		RequireAnyRole:       tenant.RequireRole(tenant.RoleStaff),
		RequireSuperAdmin:    middleware.RequireSuperAdmin,
		LoginRateLimit:       middleware.RateLimit(d.RateLimiter, "login", 10, time.Minute, middleware.ClientIP),
		SignupRateLimit:      middleware.RateLimit(d.RateLimiter, "signup", 20, time.Minute, middleware.ClientIP),
		TokenRateLimit:       middleware.RateLimit(d.RateLimiter, "oauth_token_ip", 30, time.Minute, middleware.ClientIP),
	}

	mux := http.NewServeMux()
	registerHealthRoutes(mux, healthH)
	origins := originpolicy.New(d.Config.Frontend.Origins)
	auth.RegisterRoutes(mux, mw, d.Auth, d.Google, d.Config.Auth, origins)
	tenant.RegisterRoutes(mux, mw, d.Tenants, origins)
	oauthclient.RegisterRoutes(mux, mw, d.OAuthClient, d.RateLimiter)
	storage.RegisterRoutes(mux, mw, d.Storage, d.ObjectStore)
	event.RegisterRoutes(mux, mw, d.Events, origins)
	// One unauthenticated "what is this invite for?" lookup across workspace and event invitations.
	invitepreview.RegisterRoutes(mux, mw, d.Tenants, d.Events)
	participant.RegisterRoutes(mux, mw, d.Participants, d.Events)
	generator.RegisterRoutes(mux, mw, d.Templates)
	gallery.RegisterRoutes(mux, mw, d.Gallery, d.Events)
	jobqueue.RegisterRoutes(mux, mw, d.JobQueue)

	// GET /events/lookup/{slug} cannot live on the mux (it conflicts with
	// /events/{id}/<name>), so it is dispatched ahead of it.
	lookupPrefix, lookupHandler := event.LookupHandler(mw, d.Events)
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, lookupPrefix) {
			lookupHandler.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	// CORS and CSRF share one origin allowlist: the frontends that may call
	// this API with the browser's cookies.
	browserOrigins := originpolicy.New(d.Config.CORS.AllowedOrigins)
	handler = middleware.CSRF(browserOrigins)(handler)
	handler = middleware.CORS(browserOrigins)(handler)
	handler = middleware.Recover(d.Logger)(handler)
	handler = middleware.Logging(d.Logger)(handler)
	handler = middleware.RequestID(handler)
	return handler
}
