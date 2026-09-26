package fontlib

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/security"
)

// Service implements the font library use cases. Reading is open to every
// signed-in user; every change takes a platform administrator (re-checked here
// on top of the router's gate, like the other contexts).
type Service struct {
	db    *database.DB
	repo  *Repository
	audit *audit.Repository
}

func NewService(db *database.DB, repo *Repository, audit *audit.Repository) *Service {
	return &Service{db: db, repo: repo, audit: audit}
}

// MaxFamilies is the most families the library holds.
const MaxFamilies = 200

// recordAudit writes a platform-level audit line (no workspace).
func (s *Service) recordAudit(ctx context.Context, actorUserID, action string, metadata map[string]any) error {
	return s.audit.Record(ctx, &audit.Log{
		ID: security.MustNewUUIDv4(), ActorUserID: &actorUserID, Action: action,
		Metadata: metadata, CreatedAt: time.Now().UTC(),
	})
}

func requireSuperAdmin(isSuperAdmin bool) error {
	if !isSuperAdmin {
		return domain.ErrForbidden
	}
	return nil
}

// ==================== reading ====================

// List returns the library, by family name.
func (s *Service) List(ctx context.Context) ([]Font, error) {
	return s.repo.List(ctx)
}

// File returns the bytes of a library font's style.
func (s *Service) File(ctx context.Context, fontID, style string) (*FileData, error) {
	if _, ok := generator.ParseFontStyle(style); !ok {
		return nil, domain.ErrNotFound
	}
	return s.repo.GetFile(ctx, fontID, style)
}

// ==================== changing the library ====================

// NewFile is a font file to add.
type NewFile struct {
	Style string
	Data  []byte
}

// CreateInput is a new family with at least one style.
type CreateInput struct {
	// Family is the name templates show; when empty, the name inside the first file.
	Family      string
	LicenseNote string
	Files       []NewFile
}

func cleanFamily(name string) (string, *Error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", invalid("family", "family is required.")
	}
	if utf8.RuneCountInString(name) > 100 {
		return "", invalid("family", "family is too long.")
	}
	return name, nil
}

func cleanLicense(note string) (string, *Error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) < 3 {
		return "", invalid("license_note", "say where the font's licence comes from (license_note).")
	}
	if utf8.RuneCountInString(note) > 500 {
		return "", invalid("license_note", "license_note is too long.")
	}
	return note, nil
}

// checkFile inspects a file for its style; the warnings are things the
// uploader should know but that do not stop the upload.
func checkFile(style string, data []byte) (*Info, []string, *Error) {
	if _, ok := generator.ParseFontStyle(style); !ok {
		return nil, nil, invalid("style", "style must be regular, bold, italic or bold_italic.")
	}
	info, err := Inspect(data)
	if err != nil {
		return nil, nil, invalid(style, fmt.Sprintf("%s: %s.", style, err.Error()))
	}
	var warnings []string
	fs, _ := generator.ParseFontStyle(style)
	if fs.Bold && !info.Bold && info.Weight < 600 {
		warnings = append(warnings, fmt.Sprintf("%s: the file does not look bold (weight %d).", style, info.Weight))
	}
	if !fs.Bold && (info.Bold || info.Weight >= 700) {
		warnings = append(warnings, fmt.Sprintf("%s: the file looks bold, but is filed as %s.", style, style))
	}
	if fs.Italic && !info.Italic {
		warnings = append(warnings, fmt.Sprintf("%s: the file does not look italic.", style))
	}
	if !fs.Italic && info.Italic {
		warnings = append(warnings, fmt.Sprintf("%s: the file looks italic, but is filed as %s.", style, style))
	}
	if info.LatinMissing > 0 {
		warnings = append(warnings, fmt.Sprintf("%s: %d of the 62 basic Latin characters (A-Z, a-z, 0-9) have no glyph.", style, info.LatinMissing))
	}
	return info, warnings, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Create adds a family with its files.
func (s *Service) Create(ctx context.Context, actorUserID string, isSuperAdmin bool, in CreateInput) (*Font, []string, error) {
	if err := requireSuperAdmin(isSuperAdmin); err != nil {
		return nil, nil, err
	}
	license, lerr := cleanLicense(in.LicenseNote)
	if lerr != nil {
		return nil, nil, lerr
	}
	if len(in.Files) == 0 {
		return nil, nil, invalid("files", "give at least one font file.")
	}
	seen := map[string]bool{}
	infos := make([]*Info, len(in.Files))
	var warnings []string
	for i, f := range in.Files {
		if seen[f.Style] {
			return nil, nil, invalid(f.Style, "each style may be given once.")
		}
		seen[f.Style] = true
		info, w, ferr := checkFile(f.Style, f.Data)
		if ferr != nil {
			return nil, nil, ferr
		}
		infos[i] = info
		warnings = append(warnings, w...)
	}
	family := in.Family
	if strings.TrimSpace(family) == "" {
		family = infos[0].Family
	}
	family, cerr := cleanFamily(family)
	if cerr != nil {
		return nil, nil, cerr
	}

	var created *Font
	err := s.db.WithTx(ctx, func(ctx context.Context) error {
		if all, err := s.repo.List(ctx); err != nil {
			return err
		} else if len(all) >= MaxFamilies {
			return invalid("family", fmt.Sprintf("the library is full (%d families).", MaxFamilies))
		}
		f := &Font{
			ID: security.MustNewUUIDv4(), Family: family, Status: StatusActive, LicenseNote: license,
			CreatedBy: actorUserID, CreatedAt: time.Now().UTC(),
		}
		if err := s.repo.Create(ctx, f); err != nil {
			return err
		}
		for i, file := range in.Files {
			if err := s.repo.PutFile(ctx, f.ID, file.Style, file.Data, digest(file.Data), infos[i]); err != nil {
				return err
			}
		}
		if err := s.recordAudit(ctx, actorUserID, audit.ActionFontCreated,
			map[string]any{"font_id": f.ID, "family": family, "license_note": license, "styles": len(in.Files)}); err != nil {
			return err
		}
		var err error
		created, err = s.repo.Get(ctx, f.ID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return created, warnings, nil
}

// PatchInput is an update: a nil field is left as it is.
type PatchInput struct {
	Family      *string
	Status      *string
	LicenseNote *string
}

// Update renames, activates or disables a family, or changes its licence note.
func (s *Service) Update(ctx context.Context, actorUserID string, isSuperAdmin bool, id string, in PatchInput) (*Font, error) {
	if err := requireSuperAdmin(isSuperAdmin); err != nil {
		return nil, err
	}
	var updated *Font
	err := s.db.WithTx(ctx, func(ctx context.Context) error {
		f, err := s.repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if in.Family != nil {
			name, cerr := cleanFamily(*in.Family)
			if cerr != nil {
				return cerr
			}
			f.Family = name
		}
		if in.Status != nil {
			if *in.Status != StatusActive && *in.Status != StatusDisabled {
				return invalid("status", "status must be active or disabled.")
			}
			f.Status = *in.Status
		}
		if in.LicenseNote != nil {
			note, lerr := cleanLicense(*in.LicenseNote)
			if lerr != nil {
				return lerr
			}
			f.LicenseNote = note
		}
		if err := s.repo.Update(ctx, f); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, actorUserID, audit.ActionFontUpdated,
			map[string]any{"font_id": id, "family": f.Family, "status": f.Status}); err != nil {
			return err
		}
		updated, err = s.repo.Get(ctx, id)
		return err
	})
	return updated, err
}

// Delete removes a family; refused while a template still needs it.
func (s *Service) Delete(ctx context.Context, actorUserID string, isSuperAdmin bool, id string) error {
	if err := requireSuperAdmin(isSuperAdmin); err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(ctx context.Context) error {
		f, err := s.repo.Get(ctx, id)
		if err != nil {
			return err
		}
		uses, err := s.repo.Uses(ctx, f.Key())
		if err != nil {
			return err
		}
		if len(uses) > 0 {
			return conflict("font_in_use", "", fmt.Sprintf("%d template(s) still use this font: disable it instead.", len(uses)))
		}
		if err := s.repo.Delete(ctx, id); err != nil {
			return err
		}
		return s.recordAudit(ctx, actorUserID, audit.ActionFontDeleted, map[string]any{"font_id": id, "family": f.Family})
	})
}

// PutFile adds a style to a family, or replaces its file.
func (s *Service) PutFile(ctx context.Context, actorUserID string, isSuperAdmin bool, id, style string, data []byte) (*Font, []string, error) {
	if err := requireSuperAdmin(isSuperAdmin); err != nil {
		return nil, nil, err
	}
	info, warnings, ferr := checkFile(style, data)
	if ferr != nil {
		return nil, nil, ferr
	}
	var updated *Font
	err := s.db.WithTx(ctx, func(ctx context.Context) error {
		f, err := s.repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := s.repo.PutFile(ctx, id, style, data, digest(data), info); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, actorUserID, audit.ActionFontFileSaved,
			map[string]any{"font_id": id, "family": f.Family, "style": style}); err != nil {
			return err
		}
		updated, err = s.repo.Get(ctx, id)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return updated, warnings, nil
}

// RemoveFile drops one style; refused for the last file and while a template needs the style.
func (s *Service) RemoveFile(ctx context.Context, actorUserID string, isSuperAdmin bool, id, style string) (*Font, error) {
	if err := requireSuperAdmin(isSuperAdmin); err != nil {
		return nil, err
	}
	if _, ok := generator.ParseFontStyle(style); !ok {
		return nil, invalid("style", "style must be regular, bold, italic or bold_italic.")
	}
	var updated *Font
	err := s.db.WithTx(ctx, func(ctx context.Context) error {
		f, err := s.repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if !f.HasStyle(style) {
			return domain.ErrNotFound
		}
		if len(f.Files) == 1 {
			return conflict("last_file", "style", "a family needs at least one style: delete the whole font instead.")
		}
		if inUse, err := s.repo.StyleInUse(ctx, f.Key(), style); err != nil {
			return err
		} else if inUse {
			return conflict("style_in_use", "style", "a template still needs this style.")
		}
		if err := s.repo.DeleteFile(ctx, id, style); err != nil {
			return err
		}
		if err := s.recordAudit(ctx, actorUserID, audit.ActionFontFileRemoved,
			map[string]any{"font_id": id, "family": f.Family, "style": style}); err != nil {
			return err
		}
		updated, err = s.repo.Get(ctx, id)
		return err
	})
	return updated, err
}

// Uses lists the templates that need a family.
func (s *Service) Uses(ctx context.Context, isSuperAdmin bool, id string) ([]TemplateUse, error) {
	if err := requireSuperAdmin(isSuperAdmin); err != nil {
		return nil, err
	}
	f, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.repo.Uses(ctx, f.Key())
}

// DemandItem is a font style that templates ask for and the library lacks.
type DemandItem struct {
	Family    string
	Style     string
	Templates int
	Tenants   int
}

// Demand lists what templates need that the library (and the built-in fonts)
// cannot give: a font that is not there, or a style of it that is not.
func (s *Service) Demand(ctx context.Context, isSuperAdmin bool) ([]DemandItem, error) {
	if err := requireSuperAdmin(isSuperAdmin); err != nil {
		return nil, err
	}
	rows, err := s.repo.Demand(ctx)
	if err != nil {
		return nil, err
	}
	library, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]Font, len(library))
	byName := make(map[string]Font, len(library))
	for _, f := range library {
		byKey[f.Key()] = f
		byName[strings.ToLower(f.Family)] = f
	}
	// The same (family, style) can come from several keys (or none): add up.
	type id struct{ family, style string }
	totals := map[id]*DemandItem{}
	var order []id
	for _, row := range rows {
		if satisfied(row, byKey, byName) {
			continue
		}
		k := id{strings.ToLower(row.Family), row.Style}
		item, ok := totals[k]
		if !ok {
			item = &DemandItem{Family: row.Family, Style: row.Style}
			totals[k] = item
			order = append(order, k)
		}
		item.Templates += row.Templates
		item.Tenants = max(item.Tenants, row.Tenants)
	}
	out := make([]DemandItem, len(order))
	for i, k := range order {
		out[i] = *totals[k]
	}
	return out, nil
}

// satisfied reports whether the library or the built-in fonts already have what a row asks for.
func satisfied(row DemandRow, byKey, byName map[string]Font) bool {
	switch {
	case strings.HasPrefix(row.Key, "bundled:"):
		fam, ok := generator.BundledFamilyByKey(row.Key)
		if !ok {
			return false
		}
		_, has := fam.File(row.Style)
		return has
	case strings.HasPrefix(row.Key, KeyPrefix):
		f, ok := byKey[row.Key]
		return ok && f.HasStyle(row.Style)
	}
	// No key: the template only has the name. If the library has it by now,
	// saving the template again picks it up.
	if f, ok := byName[strings.ToLower(row.Family)]; ok && f.HasStyle(row.Style) {
		return true
	}
	return false
}

// ==================== for the renderer ====================

// Provider gives the renderer the bytes of bundled and library fonts.
func (s *Service) Provider(ctx context.Context) generator.FontProvider {
	return provider{ctx: ctx, repo: s.repo}
}

type provider struct {
	ctx  context.Context
	repo *Repository
}

func (p provider) TTF(key string, style generator.FontStyle) ([]byte, error) {
	if strings.HasPrefix(key, KeyPrefix) {
		id := strings.TrimPrefix(key, KeyPrefix)
		f, err := p.repo.Get(p.ctx, id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, fmt.Errorf("%w: %s", generator.ErrFontUnknown, key)
			}
			return nil, err
		}
		file, err := p.repo.GetFile(p.ctx, id, style.Name())
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, fmt.Errorf("%w: %s (%s)", generator.ErrFontStyleMissing, f.Family, style.Name())
			}
			return nil, err
		}
		return file.Data, nil
	}
	return generator.BundledFonts{}.TTF(key, style)
}

// Check reports what a template needs that the fonts cannot give, in words the
// person who saved the template can act on; nil when everything is there.
func (s *Service) Check(ctx context.Context, needs []generator.FontNeed) []string {
	var issues []string
	seen := map[string]bool{}
	cache := map[string]*Font{}
	styleLabel := map[string]string{"regular": "Reguler", "bold": "Tebal", "italic": "Miring", "bold_italic": "Tebal Miring"}
	add := func(msg string) {
		if !seen[msg] {
			seen[msg] = true
			issues = append(issues, msg)
		}
	}
	for _, need := range needs {
		style := need.Style
		if _, ok := generator.ParseFontStyle(style); !ok {
			style = generator.StyleRegular
		}
		label := styleLabel[style]
		switch {
		case need.Key == "":
			add(fmt.Sprintf("font %q (%s) belum tersedia di library font", need.Family, label))
		case strings.HasPrefix(need.Key, "bundled:"):
			fam, ok := generator.BundledFamilyByKey(need.Key)
			if !ok {
				add(fmt.Sprintf("font %q (%s) tidak dikenal", need.Family, label))
			} else if _, has := fam.File(style); !has {
				add(fmt.Sprintf("font %q belum punya gaya %s", fam.Family, label))
			}
		case strings.HasPrefix(need.Key, KeyPrefix):
			f, ok := cache[need.Key]
			if !ok {
				f, _ = s.repo.Get(ctx, strings.TrimPrefix(need.Key, KeyPrefix))
				cache[need.Key] = f
			}
			switch {
			case f == nil:
				add(fmt.Sprintf("font %q (%s) sudah tidak ada di library font", need.Family, label))
			case !f.HasStyle(style):
				add(fmt.Sprintf("font %q belum punya gaya %s", f.Family, label))
			}
		default:
			add(fmt.Sprintf("font %q (%s) tidak dikenal", need.Family, label))
		}
	}
	return issues
}

// AdminListing is the library with how many templates need each family.
type AdminListing struct {
	Fonts     []Font
	UseCounts map[string]int
}

// AdminList returns the library for the platform's font page.
func (s *Service) AdminList(ctx context.Context, isSuperAdmin bool) (*AdminListing, error) {
	if err := requireSuperAdmin(isSuperAdmin); err != nil {
		return nil, err
	}
	fonts, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.repo.UseCounts(ctx)
	if err != nil {
		return nil, err
	}
	return &AdminListing{Fonts: fonts, UseCounts: counts}, nil
}

// KeyForFamily finds a library font by the name a template gives it.
func (s *Service) KeyForFamily(ctx context.Context, family string) (string, bool) {
	for _, b := range generator.BundledFamilies() {
		if strings.EqualFold(b.Family, family) {
			return b.Key, true
		}
	}
	f, err := s.repo.FindByFamily(ctx, family)
	if err != nil || f == nil {
		return "", false
	}
	return f.Key(), true
}
