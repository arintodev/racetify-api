//go:build integration

// Run with:
//
//	DATABASE_URL=postgres://app_user:app_user_dev_password@localhost:5432/racetify?sslmode=disable \
//	go test -tags=integration ./internal/repository/... -run TestTenantIsolation -v
//
// against a database that has already had `go run ./cmd/migrate` applied
// (see Makefile's `test-integration` target, which does both).
package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/oauthclient"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/tenant"
)

// TestTenantIsolation is the automated proof behind Phase 0 deliverable
// #3: "PostgreSQL RLS berfungsi, dibuktikan dengan unit test di mana
// 'Tenant A' tidak dapat membaca data 'Tenant B'". It exercises the exact
// app database role and WithTenantTx mechanism the running API
// server uses for every request (see internal/httpapi/middleware/tenant.go
// for the User-session path and internal/oauthclient's Service.
// ClientCredentialsGrant for the M2M path) - an M2M request and a User-
// session request reach the database through the same WithTenantTx call
// with the same tenant_id, so proving isolation once here at the
// repository layer covers both call paths; internal/httpapi has its own
// end-to-end test (test/integration/http_test.go) that additionally
// proves the HTTP-level 403s for completeness.
func TestTenantIsolation(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test (see file header for how to run it)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := database.Open(ctx, mustDBConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()

	tenants := tenant.NewRepository(db, db) // adminDB unused by the methods this test calls
	oauthClients := oauthclient.NewRepository(db, db)
	users := auth.NewRepository(db)

	// A user row is required as owner_user_id/created_by (FK constraints);
	// it is deliberately created via the unscoped users table, matching
	// how a real registration works.
	ownerID := security.MustNewUUIDv4()
	hash, _ := security.HashPassword("does-not-matter-for-this-test")
	now := time.Now().UTC()
	if err := users.CreateUser(ctx, &auth.User{
		ID: ownerID, Email: "rls-test-" + ownerID + "@example.com", PasswordHash: &hash,
		FirstName: "RLS", LastName: "Test Owner", Status: auth.UserStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	tenantA := security.MustNewUUIDv4()
	tenantB := security.MustNewUUIDv4()

	// Seed each tenant and one oauth_clients row for it, each insert made
	// from *inside that tenant's own* WithTenantTx context - exactly how
	// TenantService.CreateTenant and internal/oauthclient's Service.
	// CreateClient do it in production. If RLS were misconfigured, even
	// this setup step could silently fail to see its own tenant's row back
	// out.
	for i, tenantID := range []string{tenantA, tenantB} {
		clientID := "rls-test-client-" + tenantID
		err := db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
			if err := tenants.CreateTenant(ctx, &tenant.Tenant{
				ID: tenantID, Name: "RLS Test Tenant", Slug: "rls-test-tenant-" + tenantID,
				OwnerUserID: ownerID, Status: tenant.TenantStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
			return oauthClients.Create(ctx, &oauthclient.OAuthClient{
				ID: security.MustNewUUIDv4(), TenantID: tenantID, ClientID: clientID,
				ClientSecretHash: "unused", Name: "RLS Test Client", Scopes: []string{oauthclient.ScopeTenantRead},
				Status: oauthclient.OAuthClientStatusActive, CreatedBy: ownerID, CreatedAt: now, UpdatedAt: now,
			})
		})
		if err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
	}

	t.Run("tenant A's context only sees tenant A's oauth client", func(t *testing.T) {
		var page pagination.Page[oauthclient.OAuthClient]
		err := db.WithTenantTx(ctx, tenantA, func(ctx context.Context) error {
			var err error
			page, err = oauthClients.List(ctx, tenantA, pagination.PageParams{})
			return err
		})
		if err != nil {
			t.Fatalf("list under tenant A context: %v", err)
		}
		seen := page.Items
		if len(seen) != 1 {
			t.Fatalf("expected exactly 1 client visible under tenant A's RLS context, got %d", len(seen))
		}
		if seen[0].TenantID != tenantA {
			t.Fatalf("row leaked from another tenant: got tenant_id=%s, want %s", seen[0].TenantID, tenantA)
		}
	})

	t.Run("tenant A's context querying for tenant B's id returns nothing (the core cross-tenant-leak guard)", func(t *testing.T) {
		// This is the literal scenario the deliverable describes: even
		// though the WHERE clause explicitly asks for tenant B's rows,
		// the RLS policy - not the application's WHERE clause - is what
		// decides visibility, and the active session is tenant A's.
		var page pagination.Page[oauthclient.OAuthClient]
		err := db.WithTenantTx(ctx, tenantA, func(ctx context.Context) error {
			var err error
			page, err = oauthClients.List(ctx, tenantB, pagination.PageParams{})
			return err
		})
		if err != nil {
			t.Fatalf("list under tenant A context for tenant B id: %v", err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("CROSS-TENANT LEAK: tenant A's session saw %d row(s) belonging to tenant B", len(page.Items))
		}
	})

	t.Run("no tenant context set sees nothing at all", func(t *testing.T) {
		// db here is the same underlying connection oauthClients was built
		// from (NewRepository(db, db) above) - queried directly rather than
		// through oauthClients itself, since Repository's db field is
		// unexported to internal/oauthclient and this test now lives
		// outside that package.
		_, err := db.DB.QueryContext(ctx, `SELECT 1 FROM oauth_clients WHERE tenant_id = $1`, tenantA)
		if err != nil {
			t.Fatalf("unexpected query error: %v", err)
		}
		rows, err := db.DB.QueryContext(ctx, `SELECT client_id FROM oauth_clients`)
		if err != nil {
			t.Fatalf("query without tenant context: %v", err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			count++
		}
		if count != 0 {
			t.Fatalf("expected 0 rows visible with no app.tenant_id set, got %d", count)
		}
	})

	t.Run("tenant B's context only sees tenant B's oauth client", func(t *testing.T) {
		var page pagination.Page[oauthclient.OAuthClient]
		err := db.WithTenantTx(ctx, tenantB, func(ctx context.Context) error {
			var err error
			page, err = oauthClients.List(ctx, tenantB, pagination.PageParams{})
			return err
		})
		if err != nil {
			t.Fatalf("list under tenant B context: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0].TenantID != tenantB {
			t.Fatalf("tenant B's context did not see exactly its own row: %+v", page.Items)
		}
	})
}

func mustDBConfig(dsn string) config.DBConfig {
	return config.DBConfig{DSN: dsn, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: time.Minute}
}
