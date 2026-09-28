//go:build integration

// Integration test of the watermark bounded context against a real
// Postgres. It follows the exact fixture shape
// internal/gallery/gallery_integration_test.go uses (a throwaway database
// next to the one in DATABASE_MIGRATOR_URL, every migration applied, dropped
// afterwards) rather than test/integration/ - that directory only holds
// whole-HTTP-stack tests (test/integration/http_test.go,
// endpoints_test.go), while every bounded-context-scoped integration test
// in this codebase (gallery's, internal/repository's) lives beside the
// package it tests, guarded by the same "integration" build tag.
//
//	go test -tags=integration ./internal/watermark/... -v
package watermark

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
	"github.com/racetify/racetify-api/migrations"
)

func withDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// tenantFixture is one tenant's worth of seeded data: its own event and
// user, so cross-tenant tests have two fully independent workspaces to
// compare.
type tenantFixture struct {
	tenantID string
	eventID  string
	userID   string
}

type fixture struct {
	svc     *Service
	storage *storage.Service
	store   objectstorage.ProxyDriver
	super   *sql.DB
	a, b    tenantFixture
}

func setup(t *testing.T) *fixture {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config: %v", err)
	}
	ctx := context.Background()

	name := "watermark_it_" + strings.ReplaceAll(security.MustNewUUIDv4()[:8], "-", "")
	root, err := sql.Open("postgres", withDatabase(t, cfg.DB.MigratorDSN, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if err := root.PingContext(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		root.Close()
	})

	migratorCfg := cfg.DB
	migratorCfg.DSN = withDatabase(t, cfg.DB.MigratorDSN, name)
	migrator, err := database.Open(ctx, migratorCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { migrator.Close() })
	vars := map[string]string{
		"APP_ROLE":                 cfg.DB.AppRoleName,
		"APP_ROLE_PASSWORD":        strings.ReplaceAll(cfg.DB.AppRolePassword, "'", "''"),
		"ADMIN_ROLE":               cfg.DB.AdminRoleName,
		"ADMIN_ROLE_PASSWORD":      strings.ReplaceAll(cfg.DB.AdminRolePassword, "'", "''"),
		"STORAGE_DEFAULT_PROVIDER": "local",
	}
	if err := migrator.Migrate(ctx, database.MigrationSet{FS: migrations.FS, Dir: ".", Vars: vars}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	appCfg := cfg.DB
	appCfg.DSN = withDatabase(t, cfg.DB.DSN, name)
	appDB, err := database.Open(ctx, appCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { appDB.Close() })
	adminCfg := cfg.DB
	adminCfg.DSN = withDatabase(t, cfg.DB.AdminDSN, name)
	adminDB, err := database.Open(ctx, adminCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminDB.Close() })

	driver, err := objectstorage.NewDriver(objectstorage.Config{
		Driver: "local", RootDir: t.TempDir(),
		PresignSecret: "test-presign-secret", PublicBaseURL: "http://localhost:8080",
		EncryptionKeyHex: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}
	auditRepo := audit.NewRepository(appDB)
	storageSvc := storage.NewService(appDB, storage.NewRepository(appDB, adminDB), auditRepo, driver, cfg.Storage)

	super := migrator.DB
	f := &fixture{
		svc:     NewService(appDB, NewRepository(appDB), auditRepo, driver, cfg.Storage),
		storage: storageSvc, store: driver.(objectstorage.ProxyDriver), super: super,
	}
	seedTenant := func(label string) tenantFixture {
		t.Helper()
		tf := tenantFixture{tenantID: security.MustNewUUIDv4(), eventID: security.MustNewUUIDv4(), userID: security.MustNewUUIDv4()}
		seed := func(query string, args ...any) {
			t.Helper()
			if _, err := super.ExecContext(ctx, query, args...); err != nil {
				t.Fatalf("seed: %v\n%s", err, query)
			}
		}
		seed(`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, $3, 'Tester')`,
			tf.userID, label+"@example.com", label)
		seed(`INSERT INTO tenants (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`, tf.tenantID, label, label, tf.userID)
		seed(`INSERT INTO events (id, tenant_id, name, slug, created_by) VALUES ($1, $2, 'E', $3, $4)`, tf.eventID, tf.tenantID, "e-"+label, tf.userID)
		return tf
	}
	f.a = seedTenant("tenanta")
	f.b = seedTenant("tenantb")
	return f
}

// uploadImage stands in for the browser's upload-URL/PUT/complete flow for
// a watermark's source image: a tiny PNG, content-typed as image/png so
// checkStorage's content-type check passes.
func (f *fixture) uploadImage(t *testing.T, tenantID, key string) string {
	t.Helper()
	ctx := context.Background()
	ticket, err := f.storage.ProvisionUpload(ctx, tenantID, security.MustNewUUIDv4(), "public", key, "image/png")
	if err != nil {
		t.Fatalf("provision upload: %v", err)
	}
	body := []byte("not a real png, only content-type/status matter to checkStorage")
	sum, n, err := f.store.Put(objectstorage.BucketPublic, tenantID, key, body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := f.storage.CompletePut(ctx, tenantID, objectstorage.BucketPublic, key, sum, n); err != nil {
		t.Fatalf("complete put: %v", err)
	}
	return ticket.ObjectID
}

func validItem(storageID string) ReplaceItem {
	return ReplaceItem{
		StorageID: storageID, Name: "Sponsor", AnchorX: "right", AnchorY: "bottom",
		OffsetXPercent: 0.02, OffsetYPercent: 0.02, WidthPercent: 0.15, AspectRatio: 1.5, Opacity: 0.8, SortOrder: 0,
	}
}

// TestWatermarkTenantIsolation proves tenant A cannot list or replace tenant
// B's watermarks - neither through the service's own event-ownership check
// nor, more fundamentally, through row-level security itself.
func TestWatermarkTenantIsolation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	storageID := f.uploadImage(t, f.a.tenantID, "watermarks/a-logo.png")
	created, err := f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleAdmin, []ReplaceItem{validItem(storageID)})
	if err != nil {
		t.Fatalf("tenant a replace: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("want 1 watermark, got %d", len(created))
	}

	// Service-layer check: tenant B addressing tenant A's event id 404s
	// (EventExists filters on tenant_id AND id).
	if _, err := f.svc.List(ctx, f.b.tenantID, f.a.eventID, rbac.RoleAdmin); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("tenant b listing tenant a's event should 404, got %v", err)
	}
	if _, err := f.svc.ReplaceAll(ctx, f.b.tenantID, f.a.eventID, f.b.userID, rbac.RoleAdmin, []ReplaceItem{validItem(storageID)}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("tenant b replacing tenant a's event should 404, got %v", err)
	}

	// Database-level RLS check, bypassing the service's own event-ownership
	// guard entirely: a query scoped to tenant B's app.tenant_id must see
	// zero of tenant A's rows even when it asks by tenant A's own event id.
	appDB := f.svc.db
	var count int
	err = appDB.WithTenantTx(ctx, f.b.tenantID, func(ctx context.Context) error {
		return appDB.Q(ctx).QueryRowContext(ctx,
			`SELECT count(*) FROM watermarks WHERE event_id = $1`, f.a.eventID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("rls probe: %v", err)
	}
	if count != 0 {
		t.Fatalf("RLS should hide tenant a's watermark rows from tenant b's session, saw %d", count)
	}

	// Tenant A can still see its own row.
	list, err := f.svc.List(ctx, f.a.tenantID, f.a.eventID, rbac.RoleAdmin)
	if err != nil || len(list) != 1 {
		t.Fatalf("tenant a list: %v %+v", err, list)
	}
}

// TestWatermarkReplaceAll drives validation, role gating, and the
// delete-all/insert-all replace semantics (preserving an existing id,
// assigning a fresh one to a new layer).
func TestWatermarkReplaceAll(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	storageID := f.uploadImage(t, f.a.tenantID, "watermarks/logo.png")

	if _, err := f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleStaff, []ReplaceItem{validItem(storageID)}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("staff must not replace watermarks, got %v", err)
	}

	bad := validItem(storageID)
	bad.AnchorX = "middle"
	if _, err := f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleAdmin, []ReplaceItem{bad}); err == nil {
		t.Fatal("an invalid anchor_x must be refused")
	}
	bad = validItem(storageID)
	bad.Opacity = 1.5
	if _, err := f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleAdmin, []ReplaceItem{bad}); err == nil {
		t.Fatal("opacity out of [0,1] must be refused")
	}
	unknown := validItem(security.MustNewUUIDv4())
	if _, err := f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleAdmin, []ReplaceItem{unknown}); err == nil {
		t.Fatal("an unknown storage_id must be refused")
	}

	first, err := f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleAdmin, []ReplaceItem{validItem(storageID)})
	if err != nil || len(first) != 1 {
		t.Fatalf("first replace: %v %+v", err, first)
	}
	existingID := first[0].ID

	keep := validItem(storageID)
	keep.ID = existingID
	keep.Name = "Renamed"
	fresh := validItem(storageID)
	fresh.Name = "New Layer"
	second, err := f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleAdmin, []ReplaceItem{keep, fresh})
	if err != nil || len(second) != 2 {
		t.Fatalf("second replace: %v %+v", err, second)
	}
	var sawExisting, sawFresh bool
	for _, w := range second {
		switch w.ID {
		case existingID:
			sawExisting = true
			if w.Name != "Renamed" {
				t.Fatalf("existing id should keep its identity, got name %q", w.Name)
			}
		default:
			sawFresh = true
			if w.Name != "New Layer" {
				t.Fatalf("unexpected layer %+v", w)
			}
		}
	}
	if !sawExisting || !sawFresh {
		t.Fatalf("expected one kept id and one fresh id, got %+v", second)
	}

	// A third replace with an empty set clears everything.
	cleared, err := f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleAdmin, nil)
	if err != nil || len(cleared) != 0 {
		t.Fatalf("clearing replace: %v %+v", err, cleared)
	}
	list, err := f.svc.List(ctx, f.a.tenantID, f.a.eventID, rbac.RoleAdmin)
	if err != nil || len(list) != 0 {
		t.Fatalf("list after clear: %v %+v", err, list)
	}

	// preview_url resolves through the underlying object.
	_, err = f.svc.ReplaceAll(ctx, f.a.tenantID, f.a.eventID, f.a.userID, rbac.RoleAdmin, []ReplaceItem{validItem(storageID)})
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	list, err = f.svc.List(ctx, f.a.tenantID, f.a.eventID, rbac.RoleAdmin)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	url, _, err := f.svc.PreviewURL(list[0])
	if err != nil || url == "" {
		t.Fatalf("preview url: %v %q", err, url)
	}
}
