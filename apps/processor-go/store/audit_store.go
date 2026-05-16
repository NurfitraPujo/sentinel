package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

type AuditLog struct {
	ID           string
	Action       string
	ResourceType string
	ResourceID   *string
	ActorID      string
	Metadata     json.RawMessage
}

func (s *pgStore) PersistAuditLog(ctx context.Context, log *AuditLog) error {
	query := `
		INSERT INTO audit_logs (id, action, resource_type, resource_id, actor_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`

	_, err := s.db.Exec(ctx, query,
		log.ID,
		log.Action,
		log.ResourceType,
		log.ResourceID,
		log.ActorID,
		log.Metadata,
	)

	return err
}
