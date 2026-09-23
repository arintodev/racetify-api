package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Migrations is populated by cmd/api (or cmd/migrate) via an embed.FS of the
// repository's /migrations directory. Kept as a package-level var rather
// than a parameter everywhere so `go run` one-liners stay simple.
type MigrationSet struct {
	FS  embed.FS
	Dir string

	// Vars maps template tokens (without the surrounding "{{" "}}", e.g.
	// "APP_ROLE") to their substitution value. Every "{{TOKEN}}" occurrence
	// in a migration file's SQL text is replaced before it is executed -
	// see 0003_row_level_security.up.sql's doc comment for why this
	// exists (Postgres role names/passwords are a deployment detail, not
	// something the schema should hardcode). Building Vars safely
	// (validating identifiers, escaping password literals) is the
	// caller's job - see cmd/migrate/main.go - this package only does the
	// dumb, generic substitution.
	Vars map[string]string
}

type migrationFile struct {
	version int
	name    string
	path    string
}

// Migrate applies every *.up.sql file under set.Dir that has not yet been
// recorded in the schema_migrations table, in ascending version order, each
// inside its own transaction. It intentionally avoids a third-party
// migration library (golang-migrate et al. were unreachable from this
// sandbox's package registry) - the algorithm itself is a handful of lines.
func (db *DB) Migrate(ctx context.Context, set MigrationSet) error {
	if err := db.ensureMigrationsTable(ctx); err != nil {
		return err
	}

	files, err := loadUpMigrations(set)
	if err != nil {
		return err
	}

	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return err
	}

	for _, f := range files {
		if applied[f.version] {
			continue
		}
		sqlBytes, err := set.FS.ReadFile(f.path)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", f.path, err)
		}
		sqlText, err := substituteVars(string(sqlBytes), set.Vars)
		if err != nil {
			return fmt.Errorf("migrate: %s: %w", f.name, err)
		}
		if err := db.applyMigration(ctx, f, sqlText); err != nil {
			return fmt.Errorf("migrate: apply %s: %w", f.name, err)
		}
	}
	return nil
}

func (db *DB) ensureMigrationsTable(ctx context.Context) error {
	_, err := db.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			name        TEXT NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	return nil
}

func (db *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := db.DB.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read applied versions: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func (db *DB) applyMigration(ctx context.Context, f migrationFile, sqlText string) error {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, sqlText); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		f.version, f.name,
	); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// templateVarPattern matches a "{{TOKEN}}" placeholder in migration SQL.
var templateVarPattern = regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)

// substituteVars replaces every "{{TOKEN}}" placeholder in sqlText with
// vars[TOKEN], erroring out if a placeholder has no mapping - a silently
// unsubstituted placeholder would otherwise reach Postgres as literal,
// invalid SQL, or worse, be misread as an identifier.
func substituteVars(sqlText string, vars map[string]string) (string, error) {
	var missing error
	result := templateVarPattern.ReplaceAllStringFunc(sqlText, func(token string) string {
		name := templateVarPattern.FindStringSubmatch(token)[1]
		value, ok := vars[name]
		if !ok {
			missing = fmt.Errorf("missing value for migration template var %q", name)
			return token
		}
		return value
	})
	if missing != nil {
		return "", missing
	}
	return result, nil
}

// loadUpMigrations lists and sorts every "NNNN_name.up.sql" file in
// set.Dir. Files are named with a zero-padded numeric prefix so lexical and
// numeric ordering agree.
func loadUpMigrations(set MigrationSet) ([]migrationFile, error) {
	entries, err := fs.ReadDir(set.FS, set.Dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read dir %s: %w", set.Dir, err)
	}

	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		versionStr, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		version, err := strconv.Atoi(versionStr)
		if err != nil {
			return nil, fmt.Errorf("migrate: invalid migration filename %q: %w", e.Name(), err)
		}
		files = append(files, migrationFile{
			version: version,
			name:    e.Name(),
			path:    path.Join(set.Dir, e.Name()),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}
