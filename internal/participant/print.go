package participant

import (
	"context"
	"fmt"
)

// PrintRow is one participant with what a BIB shows besides their own fields:
// the names of their race and team, and the team's category.
type PrintRow struct {
	Participant  *Participant
	RaceName     string
	TeamName     string
	TeamCategory GenderCategory
}

// PrintRows loads the participants with these ids, for a print job that has
// already checked who may ask for it (the request went through the Admin
// gate). Ids that are not participants of the event are left out.
func (s *Service) PrintRows(ctx context.Context, tenantID, eventID string, ids []string) ([]PrintRow, error) {
	var rows []PrintRow
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		races, err := s.repo.ListRaceFormats(ctx, tenantID, eventID)
		if err != nil {
			return err
		}
		teams, err := s.repo.teamNamesAndCategories(ctx, tenantID, eventID)
		if err != nil {
			return err
		}
		clause, args := ListFilter{}.where(tenantID, eventID)
		clause, args = idsClause(clause, args, ids)
		return s.repo.forEachByClause(ctx, clause, args, func(p *Participant) error {
			row := PrintRow{Participant: p, RaceName: races[p.RaceID].Name}
			if p.TeamID != nil {
				t := teams[*p.TeamID]
				row.TeamName, row.TeamCategory = t.name, t.category
			}
			rows = append(rows, row)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("participant: print rows: %w", err)
	}
	return rows, nil
}

type teamLabel struct {
	name     string
	category GenderCategory
}

func (r *Repository) teamNamesAndCategories(ctx context.Context, tenantID, eventID string) (map[string]teamLabel, error) {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT id, name, gender_category FROM teams WHERE tenant_id = $1 AND event_id = $2`, tenantID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]teamLabel{}
	for rows.Next() {
		var id string
		var t teamLabel
		if err := rows.Scan(&id, &t.name, &t.category); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// forEachByClause streams the participants a where-clause (built by
// ListFilter.where, optionally narrowed by idsClause) matches, in BIB order.
func (r *Repository) forEachByClause(ctx context.Context, clause string, args []any, fn func(*Participant) error) error {
	rows, err := r.db.Q(ctx).QueryContext(ctx,
		`SELECT `+participantColumns+` FROM participants p WHERE `+clause+` ORDER BY p.bib_number ASC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return rows.Err()
}
