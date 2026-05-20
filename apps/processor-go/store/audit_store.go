package store

import (
	"context"
	"encoding/json"
	"log"
	"sync/atomic"
)

var auditPersistFailures int64

func RecordAuditPersistFailure() {
	atomic.AddInt64(&auditPersistFailures, 1)
}

func GetAuditPersistFailureCount() int64 {
	return atomic.LoadInt64(&auditPersistFailures)
}

type AuditLog struct {
	ID           string
	Action       string
	ResourceType string
	ResourceID   *string
	ActorID      string
	Metadata     json.RawMessage
}

func (s *pgStore) PersistAuditLog(ctx context.Context, logEntry *AuditLog) error {
	query := `
		INSERT INTO audit_logs (id, action, resource_type, resource_id, actor_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`

	_, err := s.db.Exec(ctx, query,
		logEntry.ID,
		logEntry.Action,
		logEntry.ResourceType,
		logEntry.ResourceID,
		logEntry.ActorID,
		logEntry.Metadata,
	)

	if err != nil {
		RecordAuditPersistFailure()
		log.Printf("AUDIT_PERSIST_FAILURE: failed to persist audit log %s: %v", logEntry.ID, err)
		return err
	}

	return nil
}
