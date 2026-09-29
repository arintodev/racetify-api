package portal

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/racetify/racetify-api/internal/certificate"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/event"
	"github.com/racetify/racetify-api/internal/gallery"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// Service implements the Runner Portal's read-only use cases. It has no
// actor or role of any kind to check - an event's own published status,
// and certificate_publications, are the only gates a request passes
// through.
type Service struct {
	db       *database.DB
	events   *event.Service
	repo     *Repository
	certRepo *certificate.Repository
	certSvc  *certificate.Service
	gallery  *gallery.Service
}

func NewService(db *database.DB, events *event.Service, repo *Repository, certRepo *certificate.Repository, certSvc *certificate.Service, galleryService *gallery.Service) *Service {
	return &Service{db: db, events: events, repo: repo, certRepo: certRepo, certSvc: certSvc, gallery: galleryService}
}

// resolveEvent is every method's first step: slug -> a published event, or
// a portal-shaped not-found - a draft/archived event's slug behaves
// identically to an unknown one, never distinguishing the two to a caller
// with no session.
func (s *Service) resolveEvent(ctx context.Context, slug string) (*event.Event, error) {
	ev, err := s.events.GetPublishedEventBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, notFound("Event not found.")
		}
		return nil, err
	}
	return ev, nil
}

// GetEvent returns the public event header for slug.
func (s *Service) GetEvent(ctx context.Context, slug string) (*EventDTO, error) {
	ev, err := s.resolveEvent(ctx, slug)
	if err != nil {
		return nil, err
	}
	dto := eventResponse(ev)
	return &dto, nil
}

// SearchCertificates matches BIB or name within slug's published, ready
// certificates (Repository.SearchCertificates already applies both
// gates in its WHERE clause).
func (s *Service) SearchCertificates(ctx context.Context, slug, query string) ([]CertificateMatchDTO, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, invalid("q is required.")
	}
	ev, err := s.resolveEvent(ctx, slug)
	if err != nil {
		return nil, err
	}

	var matches []CertificateMatch
	err = s.db.WithTenantTx(ctx, ev.TenantID, func(ctx context.Context) error {
		var err error
		matches, err = s.repo.SearchCertificates(ctx, ev.TenantID, ev.ID, q)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := make([]CertificateMatchDTO, len(matches))
	for i, m := range matches {
		out[i] = matchResponse(m)
	}
	return out, nil
}

// GetCertificateDownload resolves participantID's certificate to a
// time-limited file URL - only once the event's certificates are
// published and this participant's own record is 'ready'. Reuses
// internal/certificate's own repository/service rather than duplicating
// the storage/presign plumbing.
func (s *Service) GetCertificateDownload(ctx context.Context, slug, participantID string) (url string, expires *time.Time, err error) {
	ev, err := s.resolveEvent(ctx, slug)
	if err != nil {
		return "", nil, err
	}

	var cert *certificate.Certificate
	err = s.db.WithTenantTx(ctx, ev.TenantID, func(ctx context.Context) error {
		publishedAt, err := s.certRepo.PublishedAt(ctx, ev.TenantID, ev.ID)
		if err != nil {
			return err
		}
		if publishedAt == nil {
			return notFound("Certificate not found.")
		}
		cert, err = s.certRepo.GetByParticipant(ctx, ev.TenantID, ev.ID, participantID)
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", nil, notFound("Certificate not found.")
		}
		return "", nil, err
	}
	if cert.Status != certificate.StatusReady {
		return "", nil, notFound("Certificate not found.")
	}

	url, expires, err = s.certSvc.FileURL(cert)
	if err != nil {
		return "", nil, err
	}
	if url == "" {
		return "", nil, notFound("Certificate not found.")
	}
	return url, expires, nil
}

// ListAlbums returns slug's public albums (docs/media-gallery-ux.md §5's
// "Publikasi album" - Draft albums never reach this list).
func (s *Service) ListAlbums(ctx context.Context, slug string) ([]AlbumDTO, error) {
	ev, err := s.resolveEvent(ctx, slug)
	if err != nil {
		return nil, err
	}
	albums, err := s.gallery.ListPublicAlbums(ctx, ev.TenantID, ev.ID)
	if err != nil {
		return nil, err
	}
	out := make([]AlbumDTO, len(albums))
	for i := range albums {
		out[i] = albumResponse(&albums[i])
	}
	return out, nil
}

// photoPage runs f against gallery.Service.ListPhotos with PublicOnly
// forced on, then maps the result to the public-safe PhotoDTO, dropping
// any photo whose watermarked thumbnail is not ready yet (PreviewURL,
// ""). knownAlbumName, when set, is used directly as every result's
// AlbumName (ListAlbumPhotos already knows it, since f.AlbumID pins every
// result to that one album); left "" it is resolved per-photo, since a
// cross-album search (SearchPhotos) can return photos from several public
// albums.
func (s *Service) photoPage(ctx context.Context, tenantID, eventID string, f gallery.PhotoFilter, page pagination.PageParams, knownAlbumName string) (pagination.Page[PhotoDTO], error) {
	f.PublicOnly = true
	photos, err := s.gallery.ListPhotos(ctx, tenantID, eventID, f, page)
	if err != nil {
		return pagination.Page[PhotoDTO]{}, err
	}

	albumNames := map[string]string{}
	if knownAlbumName == "" && len(photos.Items) > 0 {
		// An event has "a handful" of albums (repository.go's own doc
		// comment on ListAlbums), so one extra query per search is cheap.
		albums, err := s.gallery.ListPublicAlbums(ctx, tenantID, eventID)
		if err != nil {
			return pagination.Page[PhotoDTO]{}, err
		}
		for _, a := range albums {
			albumNames[a.ID] = a.Name
		}
	}

	out := make([]PhotoDTO, 0, len(photos.Items))
	for i := range photos.Items {
		p := &photos.Items[i]
		preview, err := s.gallery.PreviewURL(p)
		if err != nil {
			return pagination.Page[PhotoDTO]{}, err
		}
		if preview == "" {
			continue
		}
		albumName := knownAlbumName
		if albumName == "" {
			albumName = albumNames[p.AlbumID]
		}
		out = append(out, PhotoDTO{ID: p.ID, AlbumID: p.AlbumID, AlbumName: albumName, PreviewURL: preview})
	}
	return pagination.Page[PhotoDTO]{Items: out, NextCursor: photos.NextCursor}, nil
}

// SearchPhotos matches bib across every public album of slug's event
// exactly as typed (docs/media-gallery-ux.md §6: "Cari No. BIB mencocokkan
// tag ocr maupun manual, apa adanya").
func (s *Service) SearchPhotos(ctx context.Context, slug, bib string, page pagination.PageParams) (pagination.Page[PhotoDTO], error) {
	if strings.TrimSpace(bib) == "" {
		return pagination.Page[PhotoDTO]{}, invalid("bib is required.")
	}
	ev, err := s.resolveEvent(ctx, slug)
	if err != nil {
		return pagination.Page[PhotoDTO]{}, err
	}
	return s.photoPage(ctx, ev.TenantID, ev.ID, gallery.PhotoFilter{BIB: bib}, page, "")
}

// ListAlbumPhotos returns one public album's photos - 404s if albumID is
// not a public album of slug's event (a draft album's id behaves like an
// unknown one).
func (s *Service) ListAlbumPhotos(ctx context.Context, slug, albumID string, page pagination.PageParams) (pagination.Page[PhotoDTO], error) {
	ev, err := s.resolveEvent(ctx, slug)
	if err != nil {
		return pagination.Page[PhotoDTO]{}, err
	}
	album, err := s.gallery.GetAlbum(ctx, ev.TenantID, ev.ID, albumID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return pagination.Page[PhotoDTO]{}, notFound("Album not found.")
		}
		return pagination.Page[PhotoDTO]{}, err
	}
	if !album.IsPublic {
		return pagination.Page[PhotoDTO]{}, notFound("Album not found.")
	}
	return s.photoPage(ctx, ev.TenantID, ev.ID, gallery.PhotoFilter{AlbumID: albumID}, page, album.Name)
}
