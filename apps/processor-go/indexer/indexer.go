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

// UserIDFromMetadata is the user-identifier key precedence ExtractSearchFields has always used —
// `user_id`, then `userId`, then `user`; a value counts only if it is a JSON string — lifted out so
// event.Normalize (sentinel-affected-users §4.3, decision 3) reads the SAME precedence for the
// affected-users rollup instead of carrying a second copy that could drift (B5). The bool reports
// whether ANY of the three keys held a string, which is what lets the rollup tell "key present with
// value """ (counted as reason=empty) from "no key at all" (nothing counted).
//
// The precedence is NOT corrected here and must not be (decision 24): an empty string at a
// higher-precedence key WINS — a producer sending `user_id: ""` beside a good `userId` is read as
// "", exactly as ExtractSearchFields has always read it. Changing that is a change to the search
// index's semantics, and belongs to a change that says so.
func UserIDFromMetadata(metadata map[string]interface{}) (string, bool) {
	if userID, ok := metadata["user_id"].(string); ok {
		return userID, true
	} else if userID, ok := metadata["userId"].(string); ok {
		return userID, true
	} else if userID, ok := metadata["user"].(string); ok {
		return userID, true
	}
	return "", false
}

// TenantIDFromMetadata is UserIDFromMetadata's tenant twin: `tenant_id`, then `tenantId`, then
// `organization_id`. Same rules, same caveat.
func TenantIDFromMetadata(metadata map[string]interface{}) (string, bool) {
	if tenantID, ok := metadata["tenant_id"].(string); ok {
		return tenantID, true
	} else if tenantID, ok := metadata["tenantId"].(string); ok {
		return tenantID, true
	} else if tenantID, ok := metadata["organization_id"].(string); ok {
		return tenantID, true
	}
	return "", false
}

func ExtractSearchFields(metadata map[string]interface{}) *SearchIndexEntry {
	entry := &SearchIndexEntry{}

	// The user/tenant precedence lives in the two functions above so the rollup reads the identical
	// list. Note what the caller does with these two fields: processEventInternal OVERWRITES
	// entry.UserID / entry.TenantID with the event's pre-normalisation, decision-5-bounded values
	// right after this call (contract §4.3, last paragraph), because by the time this function runs
	// the metadata map has been through normalizer.NormalizeMap and a UUID here reads "<UUID>".
	entry.UserID, _ = UserIDFromMetadata(metadata)
	entry.TenantID, _ = TenantIDFromMetadata(metadata)

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
