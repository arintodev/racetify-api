// Package app wires the platform layer (Postgres, Redis) and each bounded
// context's repository -> service layer into an httpapi.Deps. It exists so
// cmd/api/main.go and the HTTP integration test
// (test/integration/http_test.go) share exactly one wiring path instead
// of the test reimplementing (and risking drifting from) what production
// actually runs.
package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/event"
	"github.com/racetify/racetify-api/internal/httpapi"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/mailer"
	"github.com/racetify/racetify-api/internal/oauthclient"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/ratelimit"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
	"github.com/racetify/racetify-api/internal/tenant"
)

// App bundles the router with the resources Build opened, so the caller
// (main.go, or a test's defer) can close them.
type App struct {
	Handler httpapi.Deps
	AppDB   *database.DB
	AdminDB *database.DB
	Redis   *rediscli.Client
}

func (a *App) Close() {
	if a.AppDB != nil {
		a.AppDB.Close()
	}
	if a.AdminDB != nil {
		a.AdminDB.Close()
	}
	if a.Redis != nil {
		a.Redis.Close()
	}
}

// Build connects to every dependency and constructs httpapi.Deps. It does
// NOT run migrations (see config.DBConfig.MigratorDSN) - the caller is
// responsible for the database already being migrated.
func Build(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	appDB, err := database.Open(ctx, cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("app: connect app database: %w", err)
	}

	adminDBCfg := cfg.DB
	adminDBCfg.DSN = cfg.DB.AdminDSN
	adminDB, err := database.Open(ctx, adminDBCfg)
	if err != nil {
		appDB.Close()
		return nil, fmt.Errorf("app: connect admin database: %w", err)
	}

	redisClient, err := rediscli.New(rediscli.Config{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err != nil {
		appDB.Close()
		adminDB.Close()
		return nil, fmt.Errorf("app: connect redis: %w", err)
	}

	authRepo := auth.NewRepository(appDB)
	tenants := tenant.NewRepository(appDB, adminDB)
	oauthClients := oauthclient.NewRepository(appDB, adminDB)
	auditRepo := audit.NewRepository(appDB)
	objects := storage.NewRepository(appDB, adminDB)
	events := event.NewRepository(appDB)
	jobs := jobqueue.NewRepository(appDB, adminDB)

	tokens := security.NewTokenManager(cfg.Auth.JWTSecret, cfg.Auth.Issuer)
	mail := mailer.NewLogMailer(log)
	limiter := ratelimit.NewLimiter(redisClient)

	objectStore, err := objectstorage.NewDriver(objectstorage.Config{
		Driver:           cfg.Storage.Driver,
		RootDir:          cfg.Storage.RootDir,
		PresignSecret:    cfg.Storage.PresignSecret,
		EncryptionKeyHex: cfg.Storage.EncryptionKeyHex,
		PublicBaseURL:    cfg.Storage.PublicBaseURL,
		R2: objectstorage.R2Config{
			AccountID:       cfg.Storage.R2.AccountID,
			Endpoint:        cfg.Storage.R2.Endpoint,
			AccessKeyID:     cfg.Storage.R2.AccessKeyID,
			AccessKeySecret: cfg.Storage.R2.AccessKeySecret,
			Bucket:          cfg.Storage.R2.Bucket,
			PublicBaseURL:   cfg.Storage.R2.PublicBaseURL,
		},
	})
	if err != nil {
		appDB.Close()
		adminDB.Close()
		redisClient.Close()
		return nil, fmt.Errorf("app: init object storage (driver=%q): %w", cfg.Storage.Driver, err)
	}

	authService := auth.NewService(appDB, authRepo, auditRepo, tokens, redisClient, mail, cfg.Auth, tenants)
	googleService := auth.NewGoogleService(cfg.Google, redisClient)
	tenantService := tenant.NewService(appDB, tenants, authRepo, auditRepo, mail, cfg.Auth)
	oauthClientService := oauthclient.NewService(appDB, oauthClients, auditRepo, tokens, limiter, cfg.Auth)
	storageService := storage.NewService(appDB, objects, auditRepo, objectStore, cfg.Storage)
	eventService := event.NewService(appDB, events, auditRepo)
	jobQueue := jobqueue.NewQueue(appDB, jobs, redisClient)

	return &App{
		AppDB:   appDB,
		AdminDB: adminDB,
		Redis:   redisClient,
		Handler: httpapi.Deps{
			Config:      cfg,
			Logger:      log,
			DB:          appDB,
			AdminDB:     adminDB,
			Redis:       redisClient,
			Tokens:      tokens,
			Memberships: tenants,
			Auth:        authService,
			Google:      googleService,
			Tenants:     tenantService,
			OAuthClient: oauthClientService,
			RateLimiter: limiter,
			Storage:     storageService,
			ObjectStore: objectStore,
			Events:      eventService,
			JobQueue:    jobQueue,
		},
	}, nil
}
