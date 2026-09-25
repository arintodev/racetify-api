package gallery

import (
	"context"

	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// Repository reads and writes albums, photos and tags. Every method runs on
// the connection in ctx, which the service opens as a tenant-scoped
// transaction (WithTenantTx), so row-level security applies on top of the
// explicit tenant_id / event_id conditions used here.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// ==================== albums ====================

const albumColumns = `a.id, a.tenant_id, a.event_id, a.name, a.description, a.is_public,
	a.created_by, a.created_at, a.updated_at,
	(SELECT count(*) FROM photos p WHERE p.album_id = a.id)`

func scanAlbum(row dbutil.RowScanner) (*Album, error) {
	a := &Album{}
	err := row.Scan(&a.ID, &a.TenantID, &a.EventID, &a.Name, &a.Description, &a.IsPublic,
		&a.CreatedBy, &a.CreatedAt, &a.UpdatedAt, &a.PhotoCount)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return a, nil
}

// translateAlbumWrite turns a taken album name into a conflict the client
// can show under the name field.
func translateAlbumWrite(err error) error {
	if dbutil.IsUniqueViolation(err) {
		return conflict("album_name_taken", "name", "An album with this name already exists in this event.")
	}
	return err
}

func (r *Repository) CreateAlbum(ctx context.Context, a *Album) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO albums (id, tenant_id, event_id, name, description, is_public, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		a.ID, a.TenantID, a.EventID, a.Name, a.Description, a.IsPublic, a.CreatedBy, a.CreatedAt, a.UpdatedAt)
	return translateAlbumWrite(err)
}

func (r *Repository) GetAlbum(ctx context.Context, tenantID, eventID, id string) (*Album, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+albumColumns+` FROM albums a WHERE a.tenant_id = $1 AND a.event_id = $2 AND a.id = $3`,
		tenantID, eventID, id)
	return scanAlbum(row)
}

// ListAlbums returns every album of an event, oldest first (an event has a
// handful, so there is no pagination).
func (r *Repository) ListAlbums(ctx context.Context, tenantID, eventID string) ([]Album, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT `+albumColumns+` FROM albums a WHERE a.tenant_id = $1 AND a.event_id = $2 ORDER BY a.created_at, a.id`,
		tenantID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Album
	for rows.Next() {
		a, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (r *Repository) UpdateAlbum(ctx context.Context, a *Album) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE albums SET name = $4, description = $5, is_public = $6
		WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		a.TenantID, a.EventID, a.ID, a.Name, a.Description, a.IsPublic)
	if err != nil {
		return translateAlbumWrite(err)
	}
	return dbutil.CheckRowsAffected(res)
}

// DeleteAlbum removes the album; its photos and their tags go with it
// (ON DELETE CASCADE).
func (r *Repository) DeleteAlbum(ctx context.Context, tenantID, eventID, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`DELETE FROM albums WHERE tenant_id = $1 AND event_id = $2 AND id = $3`, tenantID, eventID, id)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}
