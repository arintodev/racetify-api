// Package dbutil holds tiny, domain-agnostic helpers shared by every
// bounded context's repository.go: mapping a database/sql error to one of
// internal/domain's sentinel errors, detecting a Postgres unique-constraint
// violation, and interpreting a zero-rows-affected UPDATE/DELETE as "not
// found". None of this carries any business-domain knowledge - it only
// understands database/sql and github.com/lib/pq's error shapes, plus the
// small, universally-shared internal/domain error vocabulary (see
// internal/domain's own doc comment for why that package stays this thin -
// genuinely shared vocabulary with no owning bounded context, same as this
// package). Before this package existed, these four helpers were private
// copies living in internal/repository/user_repo.go and used, unqualified,
// by every other file in that package; as each bounded context gets its
// own repository.go outside internal/repository, this is where they moved
// to instead of being re-duplicated per context.
package dbutil

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/domain"
)

// RowScanner is satisfied by both *sql.Row and *sql.Rows, letting a single
// scan* helper in a repository.go serve its single-row Get and multi-row
// List methods alike.
type RowScanner interface {
	Scan(dest ...any) error
}

// MapNotFound turns sql.ErrNoRows into domain.ErrNotFound (the sentinel
// every service layer checks for) and wraps anything else with a
// "repository: " prefix.
func MapNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	return fmt.Errorf("repository: %w", err)
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}

// IsForeignKeyViolation reports whether err is a Postgres foreign-key
// violation (SQLSTATE 23503) - added for Phase 1's
// internal/event.Repository.DeleteRace, where ON DELETE RESTRICT
// (migrations/0007_participants.up.sql) is what actually enforces
// implementation_guide_phase_1.md §4.1's "remove race only if no
// participants reference it", and the caller wants a clean domain.
// ErrInvalidState instead of an opaque 500 when that happens.
func IsForeignKeyViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23503"
	}
	return false
}

// CheckRowsAffected turns a zero-rows UPDATE/DELETE result into
// domain.ErrNotFound - the row the caller asked to change never existed
// (or was already outside their tenant's RLS visibility).
func CheckRowsAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}
