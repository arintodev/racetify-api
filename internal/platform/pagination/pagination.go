// Package pagination is the keyset (cursor) pagination convention shared
// by every bounded context's repository.go List method. It has zero HTTP
// or domain awareness - a plain (created_at, id) keyset codec - which is
// why it lives beside internal/platform/database and internal/platform/
// rediscli rather than inside any one bounded context.
package pagination

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PageParams is the input to every keyset-paginated List method in this
// codebase. Limit is clamped to a sane range by NormalizeLimit; Cursor is
// an opaque string returned as Page.NextCursor by a previous call, or
// empty to start from the first page.
//
// Why keyset (cursor) pagination instead of LIMIT/OFFSET: Phase 0 only has
// three small List endpoints (tenant members, invitations, OAuth clients),
// but Phase 1 adds Participant and Photo - tables the PRD explicitly
// expects at "puluhan ribu" / "ribuan" row counts per tenant, with rows
// being inserted concurrently with a client paging through them (CSV
// import upserts, live photo uploads). OFFSET pagination re-scans and
// re-sorts every skipped row on each page (O(offset) per request) and
// silently skips or repeats rows when the underlying data changes between
// pages. A keyset cursor - "give me rows after this (created_at, id)
// position" - is O(limit) per request and stable under concurrent inserts,
// at the cost of not supporting "jump to page N", which none of this API's
// consumers need. Establishing the convention now, while only three
// endpoints use it, means every Phase 1 repository can copy this
// package's pattern instead of each inventing (and each having to be
// migrated off) its own OFFSET-based List method later.
type PageParams struct {
	Limit  int
	Cursor string
}

const (
	// DefaultPageLimit is used when the caller does not specify one.
	DefaultPageLimit = 20
	// MaxPageLimit bounds how much a single page can request, regardless of
	// what the caller asks for - a public API must not let ?limit=1000000
	// turn into an unbounded query.
	MaxPageLimit = 100
)

// NormalizeLimit applies the package-wide default/max so every repository
// and handler agrees on the same bounds without reimplementing them.
func (p PageParams) NormalizeLimit() int {
	if p.Limit <= 0 {
		return DefaultPageLimit
	}
	if p.Limit > MaxPageLimit {
		return MaxPageLimit
	}
	return p.Limit
}

// Page is what every paginated List method returns. NextCursor is empty
// once the caller has reached the end of the result set - handlers surface
// that as a null/omitted "next_cursor" in the JSON response.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// Cursor is the decoded (created_at, id) keyset position a page continues
// from. Every paginated table orders by created_at DESC, id DESC - id is
// included as a tiebreaker because created_at alone is not unique (two
// rows can share a timestamp), and DESC-DESC keeps the comparison
// direction consistent for the `<` keyset predicate. Exported (unlike
// Phase 0's original repository-internal version) because every bounded
// context's own repository.go now calls EncodeCursor/DecodeCursor from
// outside this package.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// EncodeCursor packs a keyset position into the opaque token returned to
// API callers as next_cursor. This encoding is not a stable public
// contract - callers must treat the string as opaque.
func EncodeCursor(createdAt time.Time, id string) string {
	raw := fmt.Sprintf("%d|%s", createdAt.UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor reverses EncodeCursor. An empty or malformed cursor is
// treated as "start from the beginning" rather than a request error, so a
// stale or hand-edited cursor degrades gracefully instead of 400ing the
// caller for what is, from their point of view, an internal detail.
func DecodeCursor(raw string) (Cursor, bool) {
	if raw == "" {
		return Cursor{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, false
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 {
		return Cursor{}, false
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Cursor{}, false
	}
	return Cursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: parts[1]}, true
}
