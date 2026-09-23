// Command migrate applies pending SQL migrations to the configured
// database. Run it before starting the API, and it is what the
// docker-compose stack and CI pipeline invoke as a one-shot job.
package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/migrations"
)

// safeIdentifier matches a Postgres-safe, unquoted role identifier. Role
// names are substituted into migration SQL as raw identifiers (CREATE ROLE
// <name>, GRANT ... TO <name>) which cannot be parameterized the way query
// values can, so they are validated against this pattern instead - never
// interpolate an unvalidated string into that position.
var safeIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("migrate: load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	migratorCfg := cfg.DB
	migratorCfg.DSN = cfg.DB.MigratorDSN
	db, err := database.Open(ctx, migratorCfg)
	if err != nil {
		log.Fatalf("migrate: connect: %v", err)
	}
	defer db.Close()

	vars, err := migrationVars(cfg.DB, cfg.Storage.Driver)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}

	if err := db.Migrate(ctx, database.MigrationSet{FS: migrations.FS, Dir: ".", Vars: vars}); err != nil {
		log.Fatalf("migrate: apply migrations: %v", err)
	}

	log.Println("migrate: schema is up to date")
}

// migrationVars builds the {{TOKEN}} substitution map used by
// 0003_row_level_security.{up,down}.sql and, since Phase 1,
// 0005_storage_providers.up.sql. Role names are validated as safe
// identifiers (they're spliced into SQL as CREATE ROLE/GRANT targets, not
// query values); role passwords and storageDefaultProvider are escaped as
// SQL string-literal contents (doubling any embedded single quote) since
// they're substituted inside '...'.
func migrationVars(db config.DBConfig, storageDefaultProvider string) (map[string]string, error) {
	for _, name := range []string{db.AppRoleName, db.AdminRoleName} {
		if !safeIdentifier.MatchString(name) {
			return nil, fmt.Errorf("invalid DB role name %q: must match %s", name, safeIdentifier.String())
		}
	}
	return map[string]string{
		"APP_ROLE":            db.AppRoleName,
		"APP_ROLE_PASSWORD":   escapeSQLLiteral(db.AppRolePassword),
		"ADMIN_ROLE":          db.AdminRoleName,
		"ADMIN_ROLE_PASSWORD": escapeSQLLiteral(db.AdminRolePassword),
		// 0005_storage_providers.up.sql's objects.provider backfill - see
		// that migration's doc comment. This is cfg.Storage.Driver
		// ("local" or "r2" today), the one driver Phase 0 ever had active,
		// not yet a genuine multi-provider choice (docs/phase1-api-plan.md
		// §2's Registry redesign is deferred until Generator/Gallery need
		// it).
		"STORAGE_DEFAULT_PROVIDER": escapeSQLLiteral(storageDefaultProvider),
	}, nil
}

func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
