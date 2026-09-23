package oauthclient

import (
	"context"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// Repository is tenant-scoped for management operations (create/list/
// revoke, done by a Tenant Owner/Admin through the authenticated
// dashboard API) but GetByClientID runs outside any tenant context: the
// whole point of the client_credentials grant (this package's Service) is
// that the caller does not know its own tenant_id yet - the server
// discovers it FROM the matched client row and only then opens the
// tenant-scoped context for anything downstream. This mirrors the tenant
// package's InvitationRepository.GetByTokenHash pattern: the lookup key
// (client_id, globally unique and unguessable) is what is safe to read
// pre-tenant-context, not the tenant_id itself.
type Repository struct {
	db      *database.DB
	adminDB *database.DB
}

func NewRepository(db, adminDB *database.DB) *Repository {
	return &Repository{db: db, adminDB: adminDB}
}

const oauthClientColumns = `id, tenant_id, client_id, client_secret_hash, name, scopes, status, created_by, last_used_at, created_at, updated_at`

func (r *Repository) Create(ctx context.Context, c *OAuthClient) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO oauth_clients (id, tenant_id, client_id, client_secret_hash, name, scopes, status, created_by, last_used_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		c.ID, c.TenantID, c.ClientID, c.ClientSecretHash, c.Name, pq.Array(c.Scopes),
		c.Status, c.CreatedBy, c.LastUsedAt, c.CreatedAt, c.UpdatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

// List returns oauth_clients newest-first, keyset-paginated by
// (created_at, id) - see internal/platform/pagination's doc comment for why.
func (r *Repository) List(ctx context.Context, tenantID string, page pagination.PageParams) (pagination.Page[OAuthClient], error) {
	limit := page.NormalizeLimit()
	query := `SELECT ` + oauthClientColumns + ` FROM oauth_clients WHERE tenant_id = $1`
	args := []any{tenantID}

	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		query += ` AND (created_at, id) < ($2, $3)`
		args = append(args, c.CreatedAt, c.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprint(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[OAuthClient]{}, err
	}
	defer rows.Close()

	var out []OAuthClient
	for rows.Next() {
		c, err := scanOAuthClient(rows)
		if err != nil {
			return pagination.Page[OAuthClient]{}, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[OAuthClient]{}, err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return pagination.Page[OAuthClient]{Items: out, NextCursor: next}, nil
}

// GetByClientID is the pre-tenant-context lookup used by the /oauth/token
// handler, over the BYPASSRLS admin connection. See the type doc comment
// above for why this is safe.
func (r *Repository) GetByClientID(ctx context.Context, clientID string) (*OAuthClient, error) {
	row := r.adminDB.DB.QueryRowContext(ctx, `SELECT `+oauthClientColumns+` FROM oauth_clients WHERE client_id = $1`, clientID)
	return scanOAuthClient(row)
}

func (r *Repository) Revoke(ctx context.Context, tenantID, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE oauth_clients SET status = 'revoked' WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// TouchLastUsed is best-effort telemetry (never fails the request it is
// called from); runs on the admin connection for the same reason
// GetByClientID does.
func (r *Repository) TouchLastUsed(ctx context.Context, id string, at time.Time) {
	_, _ = r.adminDB.DB.ExecContext(ctx, `UPDATE oauth_clients SET last_used_at = $2 WHERE id = $1`, id, at)
}

func scanOAuthClient(row dbutil.RowScanner) (*OAuthClient, error) {
	c := &OAuthClient{}
	err := row.Scan(
		&c.ID, &c.TenantID, &c.ClientID, &c.ClientSecretHash, &c.Name, pq.Array(&c.Scopes),
		&c.Status, &c.CreatedBy, &c.LastUsedAt, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return c, nil
}
