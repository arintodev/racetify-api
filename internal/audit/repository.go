package audit

import (
	"context"
	"encoding/json"

	"github.com/racetify/racetify-api/internal/platform/database"
)

// Repository writes through db.Q(ctx), so a call made from inside a
// WithTenantTx context is written (and later only readable) tenant-scoped,
// while a call made outside any tenant context (e.g. logging
// user.registered) is written with tenant_id = NULL - see the RLS policy
// comment in migrations/0003_row_level_security.up.sql for why that split
// is enforced at the database level, not just by convention here.
type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Record(ctx context.Context, log *Log) error {
	metadata, err := json.Marshal(log.Metadata)
	if err != nil {
		return err
	}
	_, err = r.db.Q(ctx).ExecContext(ctx, `
		INSERT INTO audit_logs (id, tenant_id, actor_user_id, actor_client_id, action, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		log.ID, log.TenantID, log.ActorUserID, log.ActorClientID, log.Action, metadata, log.CreatedAt,
	)
	return err
}
