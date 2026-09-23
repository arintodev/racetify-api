// Package database wraps database/sql + the lib/pq driver and provides the
// tenant-scoped transaction helper that Row-Level Security depends on.
//
// Why lib/pq instead of pgx: this codebase was built inside a sandboxed
// environment whose network egress allowlist permits `github.com/*` (via
// git, using GOPROXY=direct) but not `golang.org/x/*`, `gopkg.in/*` or the
// Go module proxy/sumdb. pgx's dependency graph pulls in golang.org/x/crypto
// and golang.org/x/text (for SCRAM auth and text normalization), which made
// it unreachable. lib/pq is a pure, dependency-free implementation of the
// Postgres wire protocol, so it was the only maintained driver buildable
// here. It is a fine production choice for database/sql-based code; if the
// team later wants pgx's connection pooling/COPY/LISTEN niceties, swapping
// the driver only touches this package.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/config"
)

// DB is a thin wrapper around *sql.DB that adds the tenant-context helpers
// the rest of the codebase relies on for RLS enforcement.
type DB struct {
	*sql.DB
}

// Open connects to Postgres and verifies connectivity with a ping.
func Open(ctx context.Context, cfg config.DBConfig) (*DB, error) {
	sqlDB, err := sql.Open("postgres", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}

	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("database: ping: %w", err)
	}

	return &DB{DB: sqlDB}, nil
}

// tenantContextKey is a package-private type so context keys never collide
// with keys set by other packages.
type ctxKey string

const txKey ctxKey = "database:tx"

// Querier is satisfied by both *sql.DB and *sql.Tx, letting repositories
// accept either a plain pool handle or a tenant-scoped transaction without
// caring which one they got.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Q extracts the active tenant-scoped transaction from ctx if one was
// opened via WithTenantTx/WithTx, otherwise it falls back to the raw pool.
// Repositories should always call db.Q(ctx) rather than touching db.DB
// directly so tenant-scoped callers transparently run inside the RLS
// transaction.
func (db *DB) Q(ctx context.Context) Querier {
	if tx, ok := ctx.Value(txKey).(*sql.Tx); ok {
		return tx
	}
	return db.DB
}

// WithTx runs fn inside a plain (non tenant-scoped) transaction, committing
// on success and rolling back on error or panic.
func (db *DB) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin tx: %w", err)
	}
	txCtx := context.WithValue(ctx, txKey, tx)

	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(txCtx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("database: rollback after error (%v): %w", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

// WithTenantTx opens a transaction and, before running fn, sets the
// Postgres session variable `app.tenant_id` for the lifetime of that
// transaction via `SET LOCAL`. Every RLS policy in migrations/0002_rls.up.sql
// filters on `current_setting('app.tenant_id', true)`, so any query issued
// through the returned context is automatically confined to tenantID -
// regardless of which pooled physical connection database/sql hands out,
// because SET LOCAL and the query both run inside the same *sql.Tx.
//
// This is the mechanism that makes cross-tenant leakage a database-level
// guarantee rather than something every handler has to remember to filter
// for by hand.
func (db *DB) WithTenantTx(ctx context.Context, tenantID string, fn func(ctx context.Context) error) error {
	if tenantID == "" {
		return fmt.Errorf("database: WithTenantTx called with empty tenantID")
	}

	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin tx: %w", err)
	}

	// set_config(..., true) scopes the setting to the current transaction
	// (equivalent to SET LOCAL) and is parameterized, so there is no SQL
	// injection risk from tenantID.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		tx.Rollback()
		return fmt.Errorf("database: set tenant context: %w", err)
	}

	txCtx := context.WithValue(ctx, txKey, tx)

	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(txCtx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("database: rollback after error (%v): %w", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}
