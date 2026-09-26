// Package templateassets loads what a background job needs to print from a
// template: its parsed design and the PNG layers the browser drew when the
// template was saved (metadata.print.layers, object ids).
package templateassets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/generator"
	"github.com/racetify/racetify-api/internal/storage"
)

// Fonts is what loading a template needs of the font library: to check what a
// template needs, and to find a library font by the name a template gives it
// (a template written before the font was in the library names it without a key).
type Fonts interface {
	generator.FontChecker
	KeyForFamily(ctx context.Context, family string) (string, bool)
}

// Assets is a template ready to print.
type Assets struct {
	Template *generator.Template
	Artwork  *generator.Artwork
	Rasters  [][]byte
}

// Problem is something the requester can fix (the template is of the wrong
// kind, has no print layers yet, ...), as opposed to a system failure.
type Problem struct {
	Field   string
	Message string
}

func (p *Problem) Error() string { return p.Message }

func problem(message string) *Problem { return &Problem{Field: "template_id", Message: message} }

type printMetadata struct {
	Print struct {
		Layers []string `json:"layers"`
	} `json:"print"`
	// Fonts is what the template says it needs (the studio writes it on save).
	Fonts []metaFont `json:"fonts"`
}

type metaFont struct {
	Family string   `json:"family"`
	Key    *string  `json:"key"`
	Styles []string `json:"styles"`
}

// fontNeeds gathers what the template needs: what its metadata declares, and
// (the design itself being the authority on what is drawn) every text of the
// design. A font with no key is a need the library cannot meet.
func fontNeeds(ctx context.Context, fonts Fonts, md printMetadata, art *generator.Artwork) []generator.FontNeed {
	var needs []generator.FontNeed
	for _, f := range md.Fonts {
		key := ""
		if f.Key != nil {
			key = *f.Key
		} else if k, ok := fonts.KeyForFamily(ctx, f.Family); ok {
			key = k
		}
		styles := f.Styles
		if len(styles) == 0 {
			styles = []string{generator.StyleRegular}
		}
		for _, style := range styles {
			needs = append(needs, generator.FontNeed{Key: key, Family: f.Family, Style: style})
		}
	}
	for _, item := range art.Items {
		text, ok := item.(generator.TextItem)
		if !ok {
			continue
		}
		key, _ := generator.ResolveFontKey(text.FontKey, text.FontFamily)
		needs = append(needs, generator.FontNeed{
			Key: key, Family: generator.FamilyName(text.FontFamily),
			Style: generator.FontStyle{Bold: text.Bold, Italic: text.Italic}.Name(),
		})
	}
	return needs
}

// Load reads the template of the event and its print layers. kind is the
// service the template must be for.
func Load(ctx context.Context, templates *generator.Service, store *storage.Service, fonts Fonts, tenantID, eventID, templateID string, kind generator.Kind) (*Assets, error) {
	t, err := templates.Get(ctx, tenantID, eventID, templateID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, problem("template_id is not a template of this event.")
		}
		return nil, err
	}
	if t.Kind != kind {
		return nil, problem(fmt.Sprintf("template_id is not a %s template.", kind))
	}
	svg, err := store.ReadObject(ctx, tenantID, t.StorageID)
	if err != nil {
		return nil, fmt.Errorf("templateassets: read template design: %w", err)
	}
	art, err := generator.ParseTemplate(svg)
	if err != nil {
		return nil, problem("the template design cannot be read: " + err.Error())
	}
	if art.HasEmbeddedTags() {
		return nil, problem(generator.ErrEmbeddedTags.Error())
	}
	var md printMetadata
	_ = json.Unmarshal(t.Metadata, &md)
	// A text that names a font without a key gets the library's key when the
	// library has a font of that name, so a template saved before the font was
	// added prints as soon as it is.
	for i, item := range art.Items {
		text, ok := item.(generator.TextItem)
		if !ok || text.FontKey != "" {
			continue
		}
		if _, known := generator.ResolveFontKey("", text.FontFamily); known {
			continue
		}
		if key, found := fonts.KeyForFamily(ctx, generator.FamilyName(text.FontFamily)); found {
			text.FontKey = key
			art.Items[i] = text
		}
	}
	if issues := fonts.Check(ctx, fontNeeds(ctx, fonts, md, art)); len(issues) > 0 {
		return nil, problem("font template belum lengkap: " + strings.Join(issues, "; ") + ".")
	}
	if runs := art.StaticRuns(); len(md.Print.Layers) != runs {
		return nil, problem("the template has no print layers yet: open it in the editor and save it again.")
	}
	rasters := make([][]byte, 0, len(md.Print.Layers))
	for _, id := range md.Print.Layers {
		png, err := store.ReadObject(ctx, tenantID, id)
		if err != nil {
			return nil, problem("a print layer of the template is missing: open it in the editor and save it again.")
		}
		rasters = append(rasters, png)
	}
	return &Assets{Template: t, Artwork: art, Rasters: rasters}, nil
}
