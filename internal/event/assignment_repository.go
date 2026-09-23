package event

import (
	"context"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/platform/dbutil"
	"github.com/racetify/racetify-api/internal/platform/pagination"
)

// ==================== event_assignments ====================

const assignmentColumns = `id, tenant_id, event_id, user_id, label, capabilities, status, assigned_by, expires_at, created_at, updated_at`

func (r *Repository) CreateAssignment(ctx context.Context, a *EventAssignment) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO event_assignments (id, tenant_id, event_id, user_id, label, capabilities, status, assigned_by, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		a.ID, a.TenantID, a.EventID, a.UserID, a.Label, pq.Array(a.Capabilities), a.Status,
		a.AssignedBy, a.ExpiresAt, a.CreatedAt, a.UpdatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

func (r *Repository) GetAssignmentByID(ctx context.Context, tenantID, eventID, id string) (*EventAssignment, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+assignmentColumns+` FROM event_assignments WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		tenantID, eventID, id)
	return scanAssignment(row)
}

// GetAssignmentForUser looks up the caller's own assignment on one event -
// what RequireEventAccess (access.go) and the "me" self-check endpoint
// both need. Must run inside a WithTenantTx(tenantID) context: it relies
// on RLS (migrations/0011_event_assignments.up.sql) like every other
// tenant-scoped method in this file.
func (r *Repository) GetAssignmentForUser(ctx context.Context, tenantID, eventID, userID string) (*EventAssignment, error) {
	row := r.db.Q(ctx).QueryRowContext(ctx,
		`SELECT `+assignmentColumns+` FROM event_assignments WHERE tenant_id = $1 AND event_id = $2 AND user_id = $3`,
		tenantID, eventID, userID)
	return scanAssignment(row)
}

// ListAssignments returns one keyset-paginated page of an event's
// assignments, newest-first - see internal/platform/pagination's doc
// comment for why keyset rather than LIMIT/OFFSET.
func (r *Repository) ListAssignments(ctx context.Context, tenantID, eventID string, page pagination.PageParams) (pagination.Page[EventAssignment], error) {
	limit := page.NormalizeLimit()
	query := `SELECT ` + assignmentColumns + ` FROM event_assignments WHERE tenant_id = $1 AND event_id = $2`
	args := []any{tenantID, eventID}

	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		query += ` AND (created_at, id) < ($3, $4)`
		args = append(args, c.CreatedAt, c.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprint(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[EventAssignment]{}, err
	}
	defer rows.Close()

	var out []EventAssignment
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return pagination.Page[EventAssignment]{}, err
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[EventAssignment]{}, err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return pagination.Page[EventAssignment]{Items: out, NextCursor: next}, nil
}

// UpdateAssignment applies a full field update to label/capabilities/
// expires_at - Service.UpdateAssignment merges partial PATCH input onto
// the current row before calling this, same convention as
// Repository.UpdateEvent.
func (r *Repository) UpdateAssignment(ctx context.Context, a *EventAssignment) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE event_assignments SET label = $4, capabilities = $5, expires_at = $6
		WHERE tenant_id = $1 AND event_id = $2 AND id = $3`,
		a.TenantID, a.EventID, a.ID, a.Label, pq.Array(a.Capabilities), a.ExpiresAt,
	)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// RevokeAssignment is a soft revoke (status -> 'revoked'), never a hard
// delete - event-crew-access-plan.md §3 calls this out explicitly: audit
// needs the historical record of who had access during the event.
func (r *Repository) RevokeAssignment(ctx context.Context, tenantID, eventID, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE event_assignments SET status = 'revoked' WHERE tenant_id = $1 AND event_id = $2 AND id = $3 AND status = 'active'`,
		tenantID, eventID, id)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

// TenantIDForEvent resolves which tenant an event belongs to without
// requiring the caller to already have that tenant selected - the
// bootstrap step RequireEventAccess needs (docs/event-crew-access-plan.md
// §4) before it can open a WithTenantTx to check the assignment itself.
// This is the same "resolve tenant from a foreign identifier, outside any
// tenant context" shape phase1-api-plan.md §5 already documents for the
// Runner Portal's planned ResolvePublicEvent middleware. Runs on the
// BYPASSRLS admin connection since, by construction, no tenant context can
// exist yet at this point - safe because the result only ever answers
// "which tenant" (not sensitive on its own), and the actual authorization
// decision is made afterward, inside that tenant's RLS.
func (r *Repository) TenantIDForEvent(ctx context.Context, eventID string) (string, error) {
	var tenantID string
	err := r.adminDB.DB.QueryRowContext(ctx, `SELECT tenant_id FROM events WHERE id = $1`, eventID).Scan(&tenantID)
	if err != nil {
		return "", dbutil.MapNotFound(err)
	}
	return tenantID, nil
}

// MyAssignments answers "every event I'm assigned to, across every
// tenant" - GET /api/v1/me/event-assignments (event-crew-access-plan.md
// §5). Runs on the BYPASSRLS admin connection, same justification as
// internal/tenant.Repository.ListTenantsForUser: this is, by definition, a
// cross-tenant question, and it is safe to answer without a tenant context
// because the WHERE clause is pinned to userID, already authenticated via
// JWT - a caller can therefore only ever list their OWN assignments.
func (r *Repository) MyAssignments(ctx context.Context, userID string) ([]EventAssignmentWithEvent, error) {
	rows, err := r.adminDB.DB.QueryContext(ctx, `
		SELECT ea.id, ea.tenant_id, ea.event_id, ea.user_id, ea.label, ea.capabilities, ea.status,
		       ea.assigned_by, ea.expires_at, ea.created_at, ea.updated_at, e.name, e.slug
		FROM event_assignments ea
		JOIN events e ON e.id = ea.event_id
		WHERE ea.user_id = $1 AND ea.status = 'active'
		ORDER BY ea.created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EventAssignmentWithEvent
	for rows.Next() {
		var a EventAssignmentWithEvent
		if err := rows.Scan(
			&a.ID, &a.TenantID, &a.EventID, &a.UserID, &a.Label, pq.Array(&a.Capabilities), &a.Status,
			&a.AssignedBy, &a.ExpiresAt, &a.CreatedAt, &a.UpdatedAt, &a.EventName, &a.EventSlug,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAssignment(row dbutil.RowScanner) (*EventAssignment, error) {
	a := &EventAssignment{}
	err := row.Scan(
		&a.ID, &a.TenantID, &a.EventID, &a.UserID, &a.Label, pq.Array(&a.Capabilities), &a.Status,
		&a.AssignedBy, &a.ExpiresAt, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return a, nil
}

// ==================== event_invitations ====================
//
// Tenant-scoped (see the event_assignments section above for the general
// pattern), except GetEventInvitationByTokenHash, which mirrors
// internal/tenant.Repository.GetInvitationByTokenHash exactly: the
// invitee is not yet anything in this tenant, so no tenant context can
// exist to satisfy the normal RLS policy, and looking it up by the SHA-256
// hash of an unguessable 24-byte token is safe on its own terms
// (possession of the secret is the authorization).

const eventInvitationColumns = `id, tenant_id, event_id, email, label, capabilities, token_hash, invited_by, status, expires_at, assignment_expires_at, accepted_at, created_at`

func (r *Repository) CreateEventInvitation(ctx context.Context, inv *EventInvitation) error {
	_, err := r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO event_invitations (id, tenant_id, event_id, email, label, capabilities, token_hash, invited_by, status, expires_at, assignment_expires_at, accepted_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		inv.ID, inv.TenantID, inv.EventID, inv.Email, inv.Label, pq.Array(inv.Capabilities), inv.TokenHash,
		inv.InvitedBy, inv.Status, inv.ExpiresAt, inv.AssignmentExpiresAt, inv.AcceptedAt, inv.CreatedAt,
	)
	if dbutil.IsUniqueViolation(err) {
		return domain.ErrAlreadyExists
	}
	return err
}

func (r *Repository) GetEventInvitationByTokenHash(ctx context.Context, tokenHash string) (*EventInvitation, error) {
	row := r.adminDB.DB.QueryRowContext(ctx, `SELECT `+eventInvitationColumns+` FROM event_invitations WHERE token_hash = $1`, tokenHash)
	return scanEventInvitation(row)
}

// ListEventInvitations returns one keyset-paginated page of an event's
// pending/past invitations, newest-first.
func (r *Repository) ListEventInvitations(ctx context.Context, tenantID, eventID string, page pagination.PageParams) (pagination.Page[EventInvitation], error) {
	limit := page.NormalizeLimit()
	query := `SELECT ` + eventInvitationColumns + ` FROM event_invitations WHERE tenant_id = $1 AND event_id = $2`
	args := []any{tenantID, eventID}

	if c, ok := pagination.DecodeCursor(page.Cursor); ok {
		query += ` AND (created_at, id) < ($3, $4)`
		args = append(args, c.CreatedAt, c.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprint(limit+1)

	rows, err := r.db.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[EventInvitation]{}, err
	}
	defer rows.Close()

	var out []EventInvitation
	for rows.Next() {
		inv, err := scanEventInvitation(rows)
		if err != nil {
			return pagination.Page[EventInvitation]{}, err
		}
		out = append(out, *inv)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[EventInvitation]{}, err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = pagination.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return pagination.Page[EventInvitation]{Items: out, NextCursor: next}, nil
}

func (r *Repository) MarkEventInvitationAccepted(ctx context.Context, id string, acceptedAt time.Time) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE event_invitations SET status = 'accepted', accepted_at = $2 WHERE id = $1 AND status = 'pending'`,
		id, acceptedAt)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func (r *Repository) RevokeEventInvitation(ctx context.Context, tenantID, eventID, id string) error {
	res, err := r.db.Q(ctx).ExecContext(ctx, `
		UPDATE event_invitations SET status = 'revoked' WHERE id = $1 AND tenant_id = $2 AND event_id = $3 AND status = 'pending'`,
		id, tenantID, eventID)
	if err != nil {
		return err
	}
	return dbutil.CheckRowsAffected(res)
}

func scanEventInvitation(row dbutil.RowScanner) (*EventInvitation, error) {
	inv := &EventInvitation{}
	err := row.Scan(
		&inv.ID, &inv.TenantID, &inv.EventID, &inv.Email, &inv.Label, pq.Array(&inv.Capabilities), &inv.TokenHash,
		&inv.InvitedBy, &inv.Status, &inv.ExpiresAt, &inv.AssignmentExpiresAt, &inv.AcceptedAt, &inv.CreatedAt,
	)
	if err != nil {
		return nil, dbutil.MapNotFound(err)
	}
	return inv, nil
}
