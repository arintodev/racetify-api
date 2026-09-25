package participant

import (
	"context"
	"fmt"
	"time"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
)

// ImportRequest is a bulk registration. Rows arrive already parsed and typed
// by the client; this service decides what each one does.
type ImportRequest struct {
	// DefaultRaceID is used for rows that name no race of their own.
	DefaultRaceID  string
	UpdateExisting bool
	DryRun         bool
	Rows           []ImportRow
}

// ImportResult reports every row's outcome. For a dry run nothing was
// written; otherwise the rows marked create/update were applied.
type ImportResult struct {
	DryRun   bool
	Plan     Plan
	Created  int
	Updated  int
	Skipped  int
	Errors   int
	NewTeams int
}

// Import plans the rows against the event's current data and, unless it is a
// dry run, applies the acceptable ones in a single transaction. Rows with
// errors are reported and left out; the others are not affected by them.
func (s *Service) Import(ctx context.Context, tenantID, eventID, actorUserID string, actorRole rbac.MemberRole, req ImportRequest) (*ImportResult, error) {
	if err := requireRole(actorRole, rbac.RoleStaff); err != nil {
		return nil, err
	}
	if len(req.Rows) == 0 {
		return nil, invalid("rows", "rows is empty.")
	}
	if len(req.Rows) > MaxImportRows {
		return nil, invalid("rows", fmt.Sprintf("at most %d rows per import; split the file.", MaxImportRows))
	}

	var result *ImportResult
	err := s.db.WithTenantTx(ctx, tenantID, func(ctx context.Context) error {
		races, err := s.repo.ListRaceFormats(ctx, tenantID, eventID)
		if err != nil {
			return err
		}

		rows := make([]ImportRow, len(req.Rows))
		var bibs []int64
		var refs []string
		for i, r := range req.Rows {
			if r.RaceID == "" {
				r.RaceID = req.DefaultRaceID
			}
			r.BadBloodType = r.Input.Normalize()
			rows[i] = r
			if r.Input.BibNumber > 0 {
				bibs = append(bibs, int64(r.Input.BibNumber))
			}
			if r.Input.RefID != nil {
				refs = append(refs, *r.Input.RefID)
			}
		}

		existing, err := s.repo.FindForImport(ctx, tenantID, eventID, bibs, refs)
		if err != nil {
			return err
		}
		teams, err := s.repo.ListAllTeams(ctx, tenantID, eventID)
		if err != nil {
			return err
		}

		plan := PlanImport(PlanInput{
			Rows: rows, UpdateExisting: req.UpdateExisting, Races: races, Existing: existing, Teams: teams,
		})
		create, update, skip, errs := plan.Counts()
		result = &ImportResult{
			DryRun: req.DryRun, Plan: plan,
			Created: create, Updated: update, Skipped: skip, Errors: errs, NewTeams: len(plan.NewTeams),
		}
		if req.DryRun {
			return nil
		}
		if err := s.recordAudit(ctx, tenantID, actorUserID, audit.ActionParticipantImportStarted,
			map[string]any{"event_id": eventID, "rows": len(rows)}); err != nil {
			return err
		}
		if err := s.applyPlan(ctx, tenantID, eventID, rows, plan, races); err != nil {
			return err
		}
		return s.recordAudit(ctx, tenantID, actorUserID, audit.ActionParticipantImportCompleted,
			map[string]any{"event_id": eventID, "created": create, "updated": update, "skipped": skip, "errors": errs, "new_teams": len(plan.NewTeams)})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// applyPlan writes the rows the plan accepted. A unique violation here means
// a concurrent writer took a BIB or leg between planning and writing; it
// aborts the whole transaction (Postgres cannot continue after it), and the
// caller retries.
func (s *Service) applyPlan(ctx context.Context, tenantID, eventID string, rows []ImportRow, plan Plan, races map[string]RaceFormat) error {
	now := time.Now().UTC()

	newTeamIDs := map[string]string{}
	for _, pt := range plan.NewTeams {
		t := &Team{
			ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID, RaceID: pt.RaceID,
			Name: pt.Name, GenderCategory: pt.Category, Status: StatusRegistered, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.CreateTeam(ctx, t); err != nil {
			return err
		}
		newTeamIDs[pt.Key] = t.ID
	}

	for i, rp := range plan.Rows {
		if rp.Action != ActionCreate && rp.Action != ActionUpdate {
			continue
		}
		row := rows[i]
		in := row.Input
		in.TeamID, in.LegOrder = nil, row.Input.LegOrder
		if rp.Team != nil {
			id := rp.Team.ExistingID
			if id == "" {
				id = newTeamIDs[rp.Team.NewKey]
			}
			in.TeamID = &id
		}
		if !races[row.RaceID].IsRelay() {
			in.LegOrder = nil
		}

		if rp.Action == ActionCreate {
			p := &Participant{
				ID: security.MustNewUUIDv4(), TenantID: tenantID, EventID: eventID, RaceID: row.RaceID,
				TeamID: in.TeamID, LegOrder: in.LegOrder,
				FirstName: in.FirstName, LastName: in.LastName, BibName: in.BibName, BibNumber: in.BibNumber,
				Gender: in.Gender, RefID: in.RefID, Email: in.Email, Club: in.Club,
				EmergencyContactName: in.EmergencyContactName, EmergencyContactPhone: in.EmergencyContactPhone,
				BloodType: in.BloodType, Status: StatusRegistered, CreatedAt: now, UpdatedAt: now,
			}
			if err := s.repo.CreateParticipant(ctx, p); err != nil {
				return err
			}
			continue
		}

		// Update: what the file supplies replaces what is stored; a blank
		// cell in the file leaves the stored value alone rather than wiping
		// it (the file is usually a re-export from a registration system
		// that may not carry every column).
		p, err := s.repo.GetParticipant(ctx, tenantID, eventID, rp.ExistingID)
		if err != nil {
			return err
		}
		p.RaceID, p.TeamID, p.LegOrder = row.RaceID, in.TeamID, in.LegOrder
		p.FirstName, p.BibNumber, p.Gender = in.FirstName, in.BibNumber, in.Gender
		keep := func(dst **string, src *string) {
			if src != nil {
				*dst = src
			}
		}
		keep(&p.LastName, in.LastName)
		keep(&p.BibName, in.BibName)
		keep(&p.RefID, in.RefID)
		keep(&p.Email, in.Email)
		keep(&p.Club, in.Club)
		keep(&p.EmergencyContactName, in.EmergencyContactName)
		keep(&p.EmergencyContactPhone, in.EmergencyContactPhone)
		keep(&p.BloodType, in.BloodType)
		if err := s.repo.UpdateParticipant(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
