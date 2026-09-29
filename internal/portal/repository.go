package portal

import (
	"context"

	"github.com/racetify/racetify-api/internal/platform/database"
)

// Repository reads the tenant-scoped tables a portal search needs, joined
// across certificates/participants/races. It does not own
// certificates itself - internal/certificate.Repository stays the source
// of truth for a single participant's certificate row and file URL,
// reused directly by Service for the download step, so this repository
// only needs the search join query.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// SearchCertificates matches query against a participant's BIB number
// (exact) or name (prefix, either first or last or their BIB-print name),
// only among certificates that are 'ready' and whose event has published
// certificates at all - an unpublished event's certificates are invisible
// here regardless of how exact the query is, same privacy posture as the
// rest of this package.
func (r *Repository) SearchCertificates(ctx context.Context, tenantID, eventID, query string) ([]CertificateMatch, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx, `
		SELECT p.id, p.bib_number, p.first_name, p.last_name, ra.name, c.status
		FROM certificates c
		JOIN participants p ON p.id = c.participant_id AND p.tenant_id = c.tenant_id
		JOIN races ra ON ra.id = p.race_id AND ra.tenant_id = c.tenant_id
		WHERE c.tenant_id = $1 AND c.event_id = $2
			AND c.status = 'ready'
			AND EXISTS (
				SELECT 1 FROM certificate_publications cp
				WHERE cp.tenant_id = c.tenant_id AND cp.event_id = c.event_id
			)
			AND (
				p.bib_number::text = $3
				OR p.first_name ILIKE $4
				OR p.last_name ILIKE $4
				OR p.bib_name ILIKE $4
			)
		ORDER BY p.bib_number
		LIMIT 20`,
		tenantID, eventID, query, query+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []CertificateMatch{}
	for rows.Next() {
		var m CertificateMatch
		var firstName, lastName string
		if err := rows.Scan(&m.ParticipantID, &m.BibNumber, &firstName, &lastName, &m.RaceName, &m.Status); err != nil {
			return nil, err
		}
		m.FullName = firstName + " " + lastName
		out = append(out, m)
	}
	return out, rows.Err()
}
