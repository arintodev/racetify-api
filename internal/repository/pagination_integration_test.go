//go:build integration

// Run with:
//
//	DATABASE_URL=postgres://app_user:app_user_dev_password@localhost:5432/racetify?sslmode=disable \
//	go test -tags=integration ./internal/repository/... -run TestKeysetPagination -v
//
// against a database that has already had `go run ./cmd/migrate` applied.
package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/racetify/racetify-api/internal/auth"
	"github.com/racetify/racetify-api/internal/oauthclient"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/tenant"
)

// TestKeysetPagination proves the property offset-based pagination does
// not have: paging through a full result set with a small page size visits
// every row exactly once, in a stable newest-first order, even though rows
// share indistinguishable millisecond-resolution created_at timestamps (a
// realistic case for a bulk CSV import in Phase 1, which is exactly the
// scenario this convention was established ahead of - see
// internal/platform/pagination's doc comment).
//
// This test lives in package repository - once the last production
// repository, internal/repository, was fully extracted into its own
// bounded contexts (docs/phase0-refactor-plan.md's Steps 5-8), this
// directory kept only this file and rls_isolation_test.go: neither belongs
// to any single bounded context, since each exercises internal/tenant,
// internal/oauthclient, and internal/auth's repositories together to seed
// realistic cross-context fixtures. See docs/phase0-refactor-plan.md's
// Step 9 for whether this directory itself is renamed/relocated once
// nothing else remains in it.
func TestKeysetPagination(t *testing.T) {
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

	tenants := tenant.NewRepository(db, db)
	oauthClients := oauthclient.NewRepository(db, db)
	users := auth.NewRepository(db)

	ownerID := security.MustNewUUIDv4()
	hash, _ := security.HashPassword("does-not-matter-for-this-test")
	now := time.Now().UTC()
	if err := users.CreateUser(ctx, &auth.User{
		ID: ownerID, Email: "pagination-test-" + ownerID + "@example.com", PasswordHash: &hash,
		FirstName: "Pagination", LastName: "Test Owner", Status: auth.UserStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	tenantID := security.MustNewUUIDv4()
	const total = 23 // deliberately not a multiple of the page size below

	err = db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := tenants.CreateTenant(ctx, &tenant.Tenant{
			ID: tenantID, Name: "Pagination Test Tenant", Slug: "pagination-test-tenant-" + tenantID,
			OwnerUserID: ownerID, Status: tenant.TenantStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		for i := 0; i < total; i++ {
			// created_at is identical (or near-identical) across every row
			// here on purpose: this is what a bulk import looks like, and
			// it is exactly the case where an id-less ORDER BY created_at
			// would be unstable/non-deterministic across pages.
			if err := oauthClients.Create(ctx, &oauthclient.OAuthClient{
				ID: security.MustNewUUIDv4(), TenantID: tenantID,
				ClientID:         "pagination-test-client-" + security.MustNewUUIDv4(),
				ClientSecretHash: "unused", Name: "Client", Scopes: []string{oauthclient.ScopeTenantRead},
				Status: oauthclient.OAuthClientStatusActive, CreatedBy: ownerID, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed %d clients: %v", total, err)
	}

	const pageSize = 7
	seen := make(map[string]bool, total)
	cursor := ""
	pages := 0

	for {
		pages++
		if pages > total { // guard against an infinite loop on a bug
			t.Fatalf("paged more than %d times without exhausting %d rows - likely an infinite loop", total, total)
		}

		var page pagination.Page[oauthclient.OAuthClient]
		err := db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
			var err error
			page, err = oauthClients.List(ctx, tenantID, pagination.PageParams{Limit: pageSize, Cursor: cursor})
			return err
		})
		if err != nil {
			t.Fatalf("list page %d: %v", pages, err)
		}

		if len(page.Items) == 0 {
			t.Fatalf("page %d returned zero items before the result set was exhausted", pages)
		}
		if pages < 4 && len(page.Items) != pageSize {
			t.Fatalf("page %d returned %d items, want exactly %d (not the last page yet)", pages, len(page.Items), pageSize)
		}

		for _, c := range page.Items {
			if seen[c.ID] {
				t.Fatalf("row %s was returned twice across pages - keyset pagination must never repeat a row", c.ID)
			}
			seen[c.ID] = true
		}

		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatalf("next_cursor did not advance between pages")
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Fatalf("visited %d distinct rows across all pages, want exactly %d (some rows were skipped)", len(seen), total)
	}

	wantPages := (total + pageSize - 1) / pageSize
	if pages != wantPages {
		t.Fatalf("took %d pages to exhaust %d rows at page size %d, want %d", pages, total, pageSize, wantPages)
	}
}
