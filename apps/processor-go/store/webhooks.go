package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// N3b (outbound webhook delivery dispatcher): store methods over agent_webhooks (migration
// 1723300000) and the issue_activity feed (migrations 1721900000/1723200000), read for delivery
// by apps/processor-go/webhooks.Dispatcher.
//
// CONCURRENCY DESIGN — why this is optimistic, not FOR UPDATE SKIP LOCKED:
//
// The natural instinct for "many workers, one queue of due work" is to lock a row per worker so no
// two workers process the same item concurrently. That pattern only pays off when the locked
// section does no I/O beyond the database itself — here, "process a webhook" means an outbound HTTP
// POST that can legitimately take up to ~10s (client timeout) plus up to ~36s of in-tick retry
// backoff (1s + 5s + 30s, mirroring notifiers/telegram.go). Holding `FOR UPDATE SKIP LOCKED` open
// across that would mean holding a Postgres transaction, and therefore a connection out of the
// pool and a row lock, for up to ~46 seconds per webhook. With PROCESSOR_DISPATCH_INTERVAL default
// 5s and potentially many rows, that starves the pool and the tick loop under any real load,
// exactly opposite of what SKIP LOCKED is for.
//
// Instead: ListActiveWebhooks reads without any lock (a plain SELECT), and each delivery closes
// with an optimistic compare-and-swap: RecordDeliverySuccess does
// `UPDATE agent_webhooks SET last_delivered_seq=$new WHERE id=$1 AND last_delivered_seq=$old`.
// If a second processor instance already advanced the cursor between this instance's read and its
// write, the CAS affects 0 rows; the dispatcher logs and moves on rather than double-delivering a
// stale cursor forward. In the current single-processor-instance deployment (see docker-compose.yml
// — one `processor` service, no replica count) this degenerates to "the CAS always succeeds" and is
// pure insurance; it only matters the day this service is horizontally scaled. Two instances can
// race to deliver the SAME batch of events to the SAME webhook once (no HTTP-level lock — that
// would require the FOR UPDATE approach rejected above) meaning at-least-once, not exactly-once,
// delivery is the explicit contract; receivers must already be idempotent on X-Sentinel-Delivery-Id
// or the (webhookId, seq) pairs inside the payload, same as the SDK/ingestor path's redelivery story.
//
// RecordDeliveryFailure is likewise a plain UPDATE, not CAS-guarded on a previous value: it always
// increments consecutive_failures from whatever it currently is, which is what "count consecutive
// failures across possibly-racing instances" wants — the worst that happens under a race is the
// counter (and therefore the trip to status='failed') moves slightly faster than one instance alone
// would produce, never slower, and never desyncs the delivery cursor (untouched on failure).

// WebhookRow is one row of agent_webhooks, the shape ListActiveWebhooks and the CAS methods below
// operate on. Field names mirror the migration 1723300000 columns.
type WebhookRow struct {
	ID                  string
	OrganizationID      string
	AgentID             string
	URL                 string
	Secret              string
	SecretPrefix        string
	EventTypes          []string // empty means "all event types"
	Status              string
	LastDeliveredSeq    int64
	ConsecutiveFailures int
}

// WebhookEvent is one issue_activity row joined out to its issue, in the SAME shape as the
// dashboard's GET /api/agent/events feed (src/lib/db/queries/events.ts's OrgActivityEvent) so the
// two consumption paths (poll vs push) never drift.
type WebhookEvent struct {
	Seq         int64
	EventType   string
	ActorType   string
	ActorID     string
	OldValue    json.RawMessage
	NewValue    json.RawMessage
	CreatedAt   time.Time
	IssueID     string
	IssueTitle  string // issues.message — see events.ts's identical rename
	IssueStatus string
	IssueType   string
	ProjectID   string
}

// eventsLagGuardInterval mirrors dashboard-web's EVENTS_LAG_GUARD_INTERVAL (src/lib/server/
// agent-events.ts): issue_activity.seq is a bigint IDENTITY column that can commit slightly out of
// order under concurrency (see migration 1723200000's comment), so both feed consumers exclude rows
// younger than this window to give a lower-seq, still-in-flight row time to commit before either
// consumer's cursor could skip past it.
const eventsLagGuardInterval = 2 * time.Second

// WebhookStore is the subset of store operations apps/processor-go/webhooks.Dispatcher needs,
// declared as an interface (rather than depending on *pgStore directly) so the dispatcher is
// testable against a fake — see webhooks/dispatcher_test.go.
type WebhookStore interface {
	ListActiveWebhooks(ctx context.Context) ([]WebhookRow, error)
	FetchEventsForWebhook(ctx context.Context, orgID string, afterSeq int64, eventTypes []string, limit int) ([]WebhookEvent, error)
	RecordDeliverySuccess(ctx context.Context, webhookID string, oldSeq, newSeq int64) (advanced bool, err error)
	RecordDeliveryFailure(ctx context.Context, webhookID string, errMsg string, failureThreshold int) error
}

// ListActiveWebhooks returns every agent_webhooks row with status='active'. Deliberately unlocked —
// see the package-level CAS design note above.
func (s *pgStore) ListActiveWebhooks(ctx context.Context) ([]WebhookRow, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, organization_id, agent_id, url, secret, secret_prefix,
		       event_types, status, last_delivered_seq, consecutive_failures
		FROM agent_webhooks
		WHERE status = 'active'
	`)
	if err != nil {
		return nil, fmt.Errorf("list active webhooks: %w", err)
	}
	defer rows.Close()

	var out []WebhookRow
	for rows.Next() {
		var w WebhookRow
		if err := rows.Scan(&w.ID, &w.OrganizationID, &w.AgentID, &w.URL, &w.Secret, &w.SecretPrefix,
			&w.EventTypes, &w.Status, &w.LastDeliveredSeq, &w.ConsecutiveFailures); err != nil {
			return nil, fmt.Errorf("scan active webhook: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active webhooks: %w", err)
	}
	return out, nil
}

// FetchEventsForWebhook mirrors listOrgActivity's query (src/lib/db/queries/events.ts) exactly:
// same join, same seq>after / lag-guard / event_types filter, same ordering — so the two feeds
// never diverge in what they surface for the same organization.
func (s *pgStore) FetchEventsForWebhook(ctx context.Context, orgID string, afterSeq int64, eventTypes []string, limit int) ([]WebhookEvent, error) {
	query := `
		SELECT ia.seq, ia.event_type, ia.actor_type, ia.actor_id, ia.old_value, ia.new_value, ia.created_at,
		       i.id, i.message, i.status, i.issue_type, i.project_id
		FROM issue_activity ia
		INNER JOIN issues i ON i.id = ia.issue_id
		INNER JOIN projects p ON p.id = i.project_id
		WHERE p.organization_id = $1
		  AND ia.seq > $2
		  AND ia.created_at < now() - $3::interval
	`
	args := []any{orgID, afterSeq, eventsLagGuardInterval.String()}

	if len(eventTypes) > 0 {
		query += fmt.Sprintf(" AND ia.event_type = ANY($%d)", len(args)+1)
		args = append(args, eventTypes)
	}

	query += fmt.Sprintf(" ORDER BY ia.seq ASC LIMIT $%d", len(args)+1)
	args = append(args, limit)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch events for webhook: %w", err)
	}
	defer rows.Close()

	var out []WebhookEvent
	for rows.Next() {
		var e WebhookEvent
		var oldValue, newValue []byte
		if err := rows.Scan(&e.Seq, &e.EventType, &e.ActorType, &e.ActorID, &oldValue, &newValue, &e.CreatedAt,
			&e.IssueID, &e.IssueTitle, &e.IssueStatus, &e.IssueType, &e.ProjectID); err != nil {
			return nil, fmt.Errorf("scan webhook event: %w", err)
		}
		e.OldValue = oldValue
		e.NewValue = newValue
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate webhook events: %w", err)
	}
	return out, nil
}

// RecordDeliverySuccess advances the cursor via compare-and-swap (see package doc for why: this is
// the concurrency mechanism, not a lock) and resets the failure streak. advanced=false means the CAS
// affected 0 rows — another instance already moved the cursor at or past newSeq; the caller should
// log and move on, not treat it as an error.
func (s *pgStore) RecordDeliverySuccess(ctx context.Context, webhookID string, oldSeq, newSeq int64) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE agent_webhooks
		SET last_delivered_seq = $3, consecutive_failures = 0, last_attempt_at = now(), last_error = NULL
		WHERE id = $1 AND last_delivered_seq = $2
	`, webhookID, oldSeq, newSeq)
	if err != nil {
		return false, fmt.Errorf("record delivery success: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RecordDeliveryFailure increments consecutive_failures, records last_error/last_attempt_at, and
// trips status to 'failed' once the streak reaches failureThreshold. Not CAS-guarded — see package
// doc for why an unconditional increment is the correct behavior here, unlike the success path.
func (s *pgStore) RecordDeliveryFailure(ctx context.Context, webhookID string, errMsg string, failureThreshold int) error {
	_, err := s.db.Exec(ctx, `
		UPDATE agent_webhooks
		SET consecutive_failures = consecutive_failures + 1,
		    last_attempt_at = now(),
		    last_error = $2,
		    status = CASE WHEN consecutive_failures + 1 >= $3 THEN 'failed' ELSE status END
		WHERE id = $1
	`, webhookID, errMsg, failureThreshold)
	if err != nil {
		return fmt.Errorf("record delivery failure: %w", err)
	}
	return nil
}
