package indexer

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Indexer struct {
	db *pgxpool.Pool
}

func NewIndexer(db *pgxpool.Pool) *Indexer {
	return &Indexer{db: db}
}

type SearchIndexEntry struct {
	OccurrenceID string
	UserID       string
	TenantID     string
	TraceID      string
	SpanID       string
	RequestID    string
}

func (i *Indexer) IndexOccurrence(ctx context.Context, entry *SearchIndexEntry) error {
	query := `
		INSERT INTO error_search_index (occurrence_id, user_id, tenant_id, trace_id, span_id, request_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (occurrence_id) DO UPDATE SET
			user_id = COALESCE(EXCLUDED.user_id, error_search_index.user_id),
			tenant_id = COALESCE(EXCLUDED.tenant_id, error_search_index.tenant_id),
			trace_id = COALESCE(EXCLUDED.trace_id, error_search_index.trace_id),
			span_id = COALESCE(EXCLUDED.span_id, error_search_index.span_id),
			request_id = COALESCE(EXCLUDED.request_id, error_search_index.request_id)
	`

	_, err := i.db.Exec(ctx, query,
		entry.OccurrenceID,
		nullString(entry.UserID),
		nullString(entry.TenantID),
		nullString(entry.TraceID),
		nullString(entry.SpanID),
		nullString(entry.RequestID),
	)

	return err
}

func ExtractSearchFields(metadata map[string]interface{}) *SearchIndexEntry {
	entry := &SearchIndexEntry{}

	if userID, ok := metadata["user_id"].(string); ok {
		entry.UserID = userID
	} else if userID, ok := metadata["userId"].(string); ok {
		entry.UserID = userID
	} else if userID, ok := metadata["user"].(string); ok {
		entry.UserID = userID
	}

	if tenantID, ok := metadata["tenant_id"].(string); ok {
		entry.TenantID = tenantID
	} else if tenantID, ok := metadata["tenantId"].(string); ok {
		entry.TenantID = tenantID
	} else if tenantID, ok := metadata["organization_id"].(string); ok {
		entry.TenantID = tenantID
	}

	if traceID, ok := metadata["trace_id"].(string); ok {
		entry.TraceID = traceID
	} else if traceID, ok := metadata["traceId"].(string); ok {
		entry.TraceID = traceID
	} else if traceID, ok := metadata["trace-id"].(string); ok {
		entry.TraceID = traceID
	}

	if spanID, ok := metadata["span_id"].(string); ok {
		entry.SpanID = spanID
	} else if spanID, ok := metadata["spanId"].(string); ok {
		entry.SpanID = spanID
	} else if spanID, ok := metadata["span-id"].(string); ok {
		entry.SpanID = spanID
	}

	if requestID, ok := metadata["request_id"].(string); ok {
		entry.RequestID = requestID
	} else if requestID, ok := metadata["requestId"].(string); ok {
		entry.RequestID = requestID
	} else if requestID, ok := metadata["request-id"].(string); ok {
		entry.RequestID = requestID
	}

	return entry
}

func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
