package respond

import (
	"net/http"
	"strconv"

	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// PageParamsFromRequest reads the ?limit= and ?cursor= query parameters
// shared by every paginated List endpoint. An invalid or missing limit
// falls back to pagination.DefaultPageLimit rather than erroring - see
// pagination.PageParams.NormalizeLimit, which applies the same default/max
// clamp again on the repository side as a second line of defense.
func PageParamsFromRequest(r *http.Request) pagination.PageParams {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	return pagination.PageParams{
		Limit:  limit,
		Cursor: r.URL.Query().Get("cursor"),
	}
}

// ListResponseDTO is the response envelope for every paginated List
// endpoint: {"items": [...], "next_cursor": "..."}. next_cursor is omitted
// once the caller has reached the end of the result set.
type ListResponseDTO[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}
