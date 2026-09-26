package fontlib

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
)

// Repository reads and writes the font tables. They have no row-level
// security (the library is the platform's), so the ordinary connection reaches
// them; the usage queries look across every workspace's templates and so use
// the admin connection.
type Repository struct {
	db      *database.DB
	adminDB *database.DB
}

func NewRepository(db, adminDB *database.DB) *Repository {
	return &Repository{db: db, adminDB: adminDB}
}

func familyTaken(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == "fonts_family_uk"
}

const fontColumns = `f.id, f.family, f.status, f.license_note, f.created_by, f.created_at, f.updated_at`

func scanFont(row dbutil.RowScanner) (*Font, error) {
	f := &Font{}
	if err := row.Scan(&f.ID, &f.Family, &f.Status, &f.LicenseNote, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return f, nil
}

// fillFiles attaches each font's file list.
func (r *Repository) fillFiles(ctx context.Context, fonts []*Font) error {
	if len(fonts) == 0 {
		return nil
	}
	byID := make(map[string]*Font, len(fonts))
	ids := make([]string, len(fonts))
	for i, f := range fonts {
		byID[f.ID] = f
		ids[i] = f.ID
	}
	rows, err := r.db.Q(ctx).QueryContext(ctx, `
		SELECT font_id, style, size_bytes, sha256, fs_type, glyph_count FROM font_files
		WHERE font_id = ANY($1::uuid[])
		ORDER BY CASE style WHEN 'regular' THEN 0 WHEN 'bold' THEN 1 WHEN 'italic' THEN 2 ELSE 3 END`, pq.Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var file FileInfo
		if err := rows.Scan(&id, &file.Style, &file.SizeBytes, &file.SHA256, &file.FSType, &file.GlyphCount); err != nil {
			return err
		}
		byID[id].Files = append(byID[id].Files, file)
	}
	return rows.Err()
}

// List returns the library, by family name.
func (r *Repository) List(ctx context.Context) ([]Font, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx, `SELECT `+fontColumns+` FROM fonts f ORDER BY lower(f.family)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fonts []*Font
	for rows.Next() {
		f, err := scanFont(rows)
		if err != nil {
			return nil, err
		}
		fonts = append(fonts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := r.fillFiles(ctx, fonts); err != nil {
		return nil, err
	}
	out := make([]Font, len(fonts))
	for i, f := range fonts {
		out[i] = *f
	}
	return out, nil
}

func (r *Repository) Get(ctx context.Context, id string) (*Font, error) {
	f, err := scanFont(r.db.Q(ctx).QueryRowContext(ctx, `SELECT `+fontColumns+` FROM fonts f WHERE f.id = $1`, id))
	if err != nil {
		return nil, err
	}
	if err := r.fillFiles(ctx, []*Font{f}); err != nil {
		return nil, err
	}
	return f, nil
}

// FindByFamily finds a font by its name, ignoring case; nil when there is none.
func (r *Repository) FindByFamily(ctx context.Context, family string) (*Font, error) {
	f, err := scanFont(r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+fontColumns+` FROM fonts f WHERE lower(f.family) = lower($1)`, family))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if err := r.fillFiles(ctx, []*Font{f}); err != nil {
		return nil, err
	}
	return f, nil
}

// FileData is a stored font file.
type FileData struct {
	Data   []byte
	SHA256 string
}

func (r *Repository) GetFile(ctx context.Context, fontID, style string) (*FileData, error) {
	d := &FileData{}
	err := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT data, sha256 FROM font_files WHERE font_id = $1 AND style = $2`, fontID, style).Scan(&d.Data, &d.SHA256)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return d, nil
}

func (r *Repository) Create(ctx context.Context, f *Font) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO fonts (id, family, status, license_note, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		f.ID, f.Family, f.Status, f.LicenseNote, f.CreatedBy, f.CreatedAt)
	if familyTaken(err) {
		return conflict("family_taken", "family", "A font with this name is already in the library.")
	}
	return err
}

func (r *Repository) Update(ctx context.Context, f *Font) error {
	res, err := r.db.Q(ctx).ExecContext(ctx,
		`UPDATE fonts SET family = $2, status = $3, license_note = $4 WHERE id = $1`,
		f.ID, f.Family, f.Status, f.LicenseNote)
	if familyTaken(err) {
		return conflict("family_taken", "family", "A font with this name is already in the library.")
	}
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `DELETE FROM fonts WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// PutFile adds or replaces the file of a style.
func (r *Repository) PutFile(ctx context.Context, fontID, style string, data []byte, sha string, info *Info) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO font_files (font_id, style, data, size_bytes, sha256, fs_type, glyph_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (font_id, style) DO UPDATE SET
			data = EXCLUDED.data, size_bytes = EXCLUDED.size_bytes, sha256 = EXCLUDED.sha256,
			fs_type = EXCLUDED.fs_type, glyph_count = EXCLUDED.glyph_count`,
		fontID, style, data, len(data), sha, int(info.FSType), info.Glyphs)
	return err
}

func (r *Repository) DeleteFile(ctx context.Context, fontID, style string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `DELETE FROM font_files WHERE font_id = $1 AND style = $2`, fontID, style)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// ---- usage (looks across every workspace: admin connection) ----

// TemplateUse is a template that needs a font.
type TemplateUse struct {
	TemplateID   string
	TemplateName string
	Service      string
	EventName    string
	TenantName   string
	Styles       []string
}

// Uses lists the templates whose metadata.fonts names the font (by key),
// with the styles each needs.
func (r *Repository) Uses(ctx context.Context, key string) ([]TemplateUse, error) {
	contains, _ := json.Marshal([]map[string]string{{"key": key}})
	rows, err := r.adminDB.DB.QueryContext(ctx, `
		SELECT t.id, t.name, t.service, e.name, tn.name,
		       COALESCE((SELECT jsonb_agg(DISTINCT s) FROM jsonb_array_elements(t.metadata->'fonts') el,
		                 jsonb_array_elements_text(el->'styles') s WHERE el->>'key' = $2), '[]'::jsonb)
		FROM generator_templates t
		JOIN events e ON e.id = t.event_id
		JOIN tenants tn ON tn.id = t.tenant_id
		WHERE jsonb_typeof(t.metadata->'fonts') = 'array' AND t.metadata->'fonts' @> $1::jsonb
		ORDER BY tn.name, e.name, t.name`, string(contains), key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateUse
	for rows.Next() {
		var u TemplateUse
		var styles []byte
		if err := rows.Scan(&u.TemplateID, &u.TemplateName, &u.Service, &u.EventName, &u.TenantName, &styles); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(styles, &u.Styles)
		out = append(out, u)
	}
	return out, rows.Err()
}

// StyleInUse reports whether a template needs this style of the font.
func (r *Repository) StyleInUse(ctx context.Context, key, style string) (bool, error) {
	var n int
	err := r.adminDB.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM generator_templates t
		WHERE jsonb_typeof(t.metadata->'fonts') = 'array' AND EXISTS (
			SELECT 1 FROM jsonb_array_elements(t.metadata->'fonts') el, jsonb_array_elements_text(el->'styles') s
			WHERE el->>'key' = $1 AND s = $2)`, key, style).Scan(&n)
	return n > 0, err
}

// DemandRow is a (font, style) that templates ask for.
type DemandRow struct {
	Family    string
	Key       string // empty: the template named no library font
	Style     string
	Templates int
	Tenants   int
}

// Demand groups everything templates' metadata.fonts asks for.
func (r *Repository) Demand(ctx context.Context) ([]DemandRow, error) {
	rows, err := r.adminDB.DB.QueryContext(ctx, `
		SELECT COALESCE(el->>'family', ''), COALESCE(el->>'key', ''), s,
		       count(DISTINCT t.id), count(DISTINCT t.tenant_id)
		FROM generator_templates t,
		     jsonb_array_elements(CASE WHEN jsonb_typeof(t.metadata->'fonts') = 'array' THEN t.metadata->'fonts' ELSE '[]'::jsonb END) el,
		     jsonb_array_elements_text(CASE WHEN jsonb_typeof(el->'styles') = 'array' THEN el->'styles' ELSE '[]'::jsonb END) s
		GROUP BY 1, 2, 3
		ORDER BY 1, 3`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DemandRow
	for rows.Next() {
		var d DemandRow
		if err := rows.Scan(&d.Family, &d.Key, &d.Style, &d.Templates, &d.Tenants); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UseCounts is how many templates need each font key.
func (r *Repository) UseCounts(ctx context.Context) (map[string]int, error) {
	rows, err := r.adminDB.DB.QueryContext(ctx, `
		SELECT el->>'key', count(DISTINCT t.id)
		FROM generator_templates t,
		     jsonb_array_elements(CASE WHEN jsonb_typeof(t.metadata->'fonts') = 'array' THEN t.metadata->'fonts' ELSE '[]'::jsonb END) el
		WHERE el->>'key' IS NOT NULL
		GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		out[key] = n
	}
	return out, rows.Err()
}
