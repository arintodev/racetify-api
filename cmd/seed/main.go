// Command seed injects a platform super admin, plus a tenant with its owner,
// for local development and demos. It is idempotent: re-running it updates
// the seeded users' passwords and leaves existing rows in place.
//
// It connects with DATABASE_ADMIN_URL (the BYPASSRLS admin role) because
// tenant_members is FORCE ROW LEVEL SECURITY and the seed has no tenant
// context to satisfy the policy with. Never point this at a production DB.
//
// Override the defaults with env vars:
//
//	SEED_ADMIN_EMAIL, SEED_ADMIN_PASSWORD, SEED_ADMIN_FIRST_NAME, SEED_ADMIN_LAST_NAME
//	SEED_OWNER_EMAIL, SEED_OWNER_PASSWORD, SEED_OWNER_FIRST_NAME, SEED_OWNER_LAST_NAME
//	SEED_TENANT_NAME, SEED_TENANT_SLUG
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/security"
)

type seedUser struct {
	email, password, firstName, lastName string
	superAdmin                           bool
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("seed: load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminCfg := cfg.DB
	adminCfg.DSN = cfg.DB.AdminDSN
	db, err := database.Open(ctx, adminCfg)
	if err != nil {
		log.Fatalf("seed: connect: %v", err)
	}
	defer db.Close()

	admin := seedUser{
		email:      getEnv("SEED_ADMIN_EMAIL", "admin@racetify.com"),
		password:   getEnv("SEED_ADMIN_PASSWORD", "Admin12345!"),
		firstName:  getEnv("SEED_ADMIN_FIRST_NAME", "Platform"),
		lastName:   getEnv("SEED_ADMIN_LAST_NAME", "Admin"),
		superAdmin: true,
	}
	owner := seedUser{
		email:     getEnv("SEED_OWNER_EMAIL", "owner@racetify.com"),
		password:  getEnv("SEED_OWNER_PASSWORD", "Owner12345!"),
		firstName: getEnv("SEED_OWNER_FIRST_NAME", "Tenant"),
		lastName:  getEnv("SEED_OWNER_LAST_NAME", "Owner"),
	}
	tenantName := getEnv("SEED_TENANT_NAME", "Demo Organizer")
	tenantSlug := getEnv("SEED_TENANT_SLUG", "demo-organizer")

	err = db.WithTx(ctx, func(ctx context.Context) error {
		q := db.Q(ctx)

		if _, err := upsertUser(ctx, q, admin); err != nil {
			return fmt.Errorf("platform admin: %w", err)
		}
		ownerID, err := upsertUser(ctx, q, owner)
		if err != nil {
			return fmt.Errorf("tenant owner: %w", err)
		}
		tenantID, err := upsertTenant(ctx, q, tenantName, tenantSlug, ownerID)
		if err != nil {
			return fmt.Errorf("tenant: %w", err)
		}
		return upsertOwnerMembership(ctx, q, tenantID, ownerID)
	})
	if err != nil {
		log.Fatalf("seed: %v", err)
	}

	log.Println("seed: done")
	log.Printf("  platform admin: %s / %s", admin.email, admin.password)
	log.Printf("  tenant owner:   %s / %s (tenant %q, slug %q)", owner.email, owner.password, tenantName, tenantSlug)
}

func upsertUser(ctx context.Context, q database.Querier, u seedUser) (string, error) {
	hash, err := security.HashPassword(u.password)
	if err != nil {
		return "", err
	}
	id, err := security.NewUUIDv4()
	if err != nil {
		return "", err
	}

	var userID string
	err = q.QueryRowContext(ctx, `
		INSERT INTO users (id, email, password_hash, first_name, last_name,
		                   is_email_verified, is_super_admin, status)
		VALUES ($1, $2, $3, $4, $5, true, $6, 'active')
		ON CONFLICT (email) DO UPDATE SET
			password_hash     = EXCLUDED.password_hash,
			first_name        = EXCLUDED.first_name,
			last_name         = EXCLUDED.last_name,
			is_email_verified = true,
			is_super_admin    = users.is_super_admin OR EXCLUDED.is_super_admin,
			status            = 'active'
		RETURNING id`,
		id, u.email, hash, u.firstName, u.lastName, u.superAdmin,
	).Scan(&userID)
	return userID, err
}

func upsertTenant(ctx context.Context, q database.Querier, name, slug, ownerID string) (string, error) {
	id, err := security.NewUUIDv4()
	if err != nil {
		return "", err
	}

	var tenantID string
	err = q.QueryRowContext(ctx, `
		INSERT INTO tenants (id, name, slug, owner_user_id, status)
		VALUES ($1, $2, $3, $4, 'active')
		ON CONFLICT (slug) DO UPDATE SET
			name          = EXCLUDED.name,
			owner_user_id = EXCLUDED.owner_user_id,
			status        = 'active',
			status_reason = NULL
		RETURNING id`,
		id, name, slug, ownerID,
	).Scan(&tenantID)
	return tenantID, err
}

func upsertOwnerMembership(ctx context.Context, q database.Querier, tenantID, userID string) error {
	id, err := security.NewUUIDv4()
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO tenant_members (id, tenant_id, user_id, role, status)
		VALUES ($1, $2, $3, 'owner', 'active')
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET
			role   = 'owner',
			status = 'active'`,
		id, tenantID, userID,
	)
	return err
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
