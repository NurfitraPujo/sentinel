package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/degradation"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/indexer"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ProcessorService struct {
	db          *pgxpool.Pool
	store       store.IssueStore
	indexer     *indexer.Indexer
	degradation *degradation.GracefulDegradation
	alerts      *alerts.Dispatcher
}

func NewProcessorService(db *pgxpool.Pool) *ProcessorService {
	svc := &ProcessorService{
		db:      db,
		store:   store.NewStore(db),
		indexer: indexer.NewIndexer(db),
	}

	svc.degradation = degradation.NewGracefulDegradation(func(ctx context.Context) bool {
		return db.Ping(ctx) == nil
	})
	// Replay buffered events through the same processing path a live event
	// takes, but never by processEventInternal calling back into the
	// degradation package itself — see triggerAsyncFlush's doc comment
	// (VERIFIED_STATE.md S9: the old code's Flush call at the end of
	// processEventInternal was a re-entrant call back into itself).
	svc.degradation.SetFlushHandler(func(data []byte) error {
		return svc.processEventInternal(context.Background(), data)
	})

	// Dispatcher construction + wiring (VERIFIED_STATE.md S8): previously
	// NewProcessorService never built a Dispatcher at all, so 425 LOC of
	// alerting code — covered by ~1,100 lines of passing tests — was never
	// reached by the running binary. NewDispatcher itself now performs a
	// synchronous initial config load (see alerts.NewDispatcher), and
	// SetSender connects sendAlert to the real email/telegram notifiers
	// instead of leaving it logging "ALERT: ..." and nothing else.
	svc.alerts = alerts.NewDispatcher(db)
	svc.alerts.SetSender(alerts.BuildSender(alerts.NotifierConfigFromEnv()))

	return svc
}

// Alerts exposes the processor's alert dispatcher so operational tooling and
// tests can install a custom sender (see alerts.Dispatcher.SetSender)
// without needing package-internal access. Production wiring already calls
// SetSender with alerts.BuildSender(alerts.NotifierConfigFromEnv()) inside
// NewProcessorService; this accessor exists for callers — notably
// tests/integration's execution proof for P5-1/S8 — that need to observe or
// override that wiring.
func (s *ProcessorService) Alerts() *alerts.Dispatcher {
	return s.alerts
}

// ProcessEvent is the entry point the NATS subscriber calls per message. Its
// return value controls Ack/Nak: nil Acks, a non-nil error Naks (subject to
// the subscriber's MaxDeliver/backoff/DLQ policy — see
// packages/shared-go/nats/subscriber.go and D10 in docs/memory/DECISIONS.md).
//
// It must branch on all three degradation.BufferStatus values
// (VERIFIED_STATE.md S9): the previous single-bool CheckAndBuffer conflated
// "database healthy" and "database down but buffered" into the same `true`,
// so a down-but-not-full database still fell through to a live
// processEventInternal call that was certain to fail, and a full buffer was
// logged as "buffered" and then silently ACKed and lost.
func (s *ProcessorService) ProcessEvent(ctx context.Context, data []byte) error {
	switch s.degradation.Evaluate(ctx, data) {
	case degradation.StatusProcessed:
		return s.processEventInternal(ctx, data)
	case degradation.StatusBuffered, degradation.StatusUnavailable:
		// Do NOT ACK. The database is down and this event has not been stored anywhere durable.
		// Returning an error keeps it in JetStream, where D10's bounded retry with backoff
		// (1s/5s/15s/30s/60s, MaxDeliver 5) owns recovery and a DLQ entry preserves anything that
		// outlasts the budget. ACKing here — which the first version of this fix did, on the
		// grounds that the event "was buffered" — silently destroyed events on a process restart,
		// because the buffer is process memory. That traded a duplicate-delivery bug for a
		// permanent-loss bug, which is strictly worse.
		return fmt.Errorf("database unavailable: event returned to NATS for bounded retry")
	case degradation.StatusDropped:
		// The buffer is full: this event was neither stored nor buffered.
		// The previous code ACKed here anyway (S9's core inversion) — a
		// silent, unrecoverable data loss reported as success. Returning an
		// error instead lets the NATS subscriber's own bounded retry with
		// backoff (and eventual DLQ dead-letter once MaxDeliver is
		// exhausted — D10) take over: the event is preserved, not lost,
		// even though it could not be handled by the buffer.
		return fmt.Errorf("event buffer full and database unavailable: event not processed")
	default:
		return fmt.Errorf("unknown degradation status")
	}
}

func (s *ProcessorService) VerifyAuditLogTable(ctx context.Context) error {
	query := `INSERT INTO audit_logs (id, action, resource_type, actor_id, metadata) VALUES ('00000000-0000-0000-0000-000000000000', 'verification_test', 'test', 'processor-go', '{}') ON CONFLICT DO NOTHING`
	_, err := s.db.Exec(ctx, query)
	return err
}

func (s *ProcessorService) processEventInternal(ctx context.Context, data []byte) error {
	evt, err := event.Deserialize(data)
	if err != nil {
		// A deserialize failure (malformed proto, a required field missing)
		// is a property of these exact bytes: redelivering the same message
		// will fail identically forever, so it must not spend its whole
		// MaxDeliver budget being retried (VERIFIED_STATE.md S13).
		log.Printf("Failed to deserialize event: %v", err)
		return nats.Permanent(err)
	}

	log.Printf("Processing event: project=%s, project_id=%s, error_class=%s, fingerprint=%s, release_version=%s",
		evt.ProjectKey, evt.ProjectID, evt.ErrorClass, evt.Fingerprint, evt.ReleaseVersion)

	projectID, err := s.store.ResolveProjectID(ctx, evt.ProjectID, evt.ProjectKey)
	if err != nil {
		log.Printf("Failed to resolve project: %v", err)
		return classifyProjectLookupError(err)
	}

	issue := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: evt.Fingerprint,
		Message:     evt.Message,
		ErrorClass:  evt.ErrorClass,
		Status:      "unresolved",
		FirstSeen:   evt.Timestamp,
		LastSeen:    evt.Timestamp,
	}

	outcome, err := s.store.UpsertIssueWithOutcome(ctx, issue, evt.ReleaseVersion)
	if err != nil {
		log.Printf("Failed to upsert issue: %v", err)
		return classifyStoreError(err)
	}

	s.store.PersistAuditLog(ctx, &store.AuditLog{
		ID:           uuid.New().String(),
		Action:       "issue_upserted",
		ResourceType: "issue",
		ResourceID:   &issue.ID,
		ActorID:      "processor-go",
		Metadata:     []byte(fmt.Sprintf(`{"fingerprint": "%s", "project_id": "%s"}`, issue.Fingerprint, issue.ProjectID)),
	})

	issueID, err := s.store.GetIssueIDByFingerprint(ctx, projectID, evt.Fingerprint)
	if err != nil {
		log.Printf("Failed to get issue ID: %v", err)
		return classifyStoreError(err)
	}

	// Alert on NEW issues and REGRESSIONS, not on every occurrence of an
	// already-known, still-unresolved issue (VERIFIED_STATE.md S8,
	// docs/plans/E2E_RECOVERY_PLAN.md P5-1 item 2). outcome comes from
	// UpsertIssueWithOutcome above, which is the only place that already
	// distinguishes these cases.
	// Dispatch on EVERY occurrence, not only new/regressed issues.
	//
	// Dispatcher.Dispatch is itself the rate limiter: it increments a per-project:issue counter
	// inside a frequency window and only sends once the count reaches alert_configs
	// .frequency_threshold. Gating on new-or-regressed here meant the counter could never exceed 1,
	// because an issue is new exactly once — while frequency_threshold is NOT NULL DEFAULT 50. The
	// two mechanisms cancelled out and no realistic configuration could ever produce an alert.
	//
	// Feeding every occurrence in is what the threshold is FOR: "tell me when this error happens 50
	// times in an hour" is the product behavior, and a threshold of 1 still gives alert-on-first-sight.
	s.dispatchAlert(ctx, issueID, projectID, evt.ErrorClass, evt.Message)
	_ = outcome

	stacktraceJSON, _ := json.Marshal(evt.Stacktrace)
	metadataJSON, _ := json.Marshal(evt.Metadata)

	occ := &store.ErrorOccurrence{
		ID:             uuid.New().String(),
		IssueID:        issueID,
		Environment:    evt.Environment,
		Platform:       evt.Platform,
		ReleaseVersion: evt.ReleaseVersion,
		Stacktrace:     stacktraceJSON,
		Metadata:       metadataJSON,
		TraceID:        evt.TraceID,
		SpanID:         evt.SpanID,
		CreatedAt:      evt.Timestamp,
	}

	if err := s.store.InsertOccurrence(ctx, occ); err != nil {
		log.Printf("Failed to insert occurrence: %v", err)
		return classifyStoreError(err)
	}

	s.store.PersistAuditLog(ctx, &store.AuditLog{
		ID:           uuid.New().String(),
		Action:       "occurrence_created",
		ResourceType: "error_occurrence",
		ResourceID:   &occ.ID,
		ActorID:      "processor-go",
		Metadata:     []byte(fmt.Sprintf(`{"issue_id": "%s", "environment": "%s"}`, occ.IssueID, occ.Environment)),
	})

	searchEntry := indexer.ExtractSearchFields(evt.Metadata)
	searchEntry.OccurrenceID = occ.ID
	if searchEntry.TraceID == "" {
		searchEntry.TraceID = evt.TraceID
	}
	if searchEntry.SpanID == "" {
		searchEntry.SpanID = evt.SpanID
	}

	if err := s.indexer.IndexOccurrence(ctx, searchEntry); err != nil {
		log.Printf("Failed to index occurrence: %v", err)
	}

	// Flushing buffered events used to happen here, at the end of every
	// successful event — a call from processEventInternal back into
	// degradation.Flush, which itself calls processEventInternal again for
	// each buffered event: a re-entrant call back into itself
	// (VERIFIED_STATE.md S9). Flushing is now triggered exclusively by
	// GracefulDegradation itself, on its own goroutine, the moment it
	// observes a down->up transition — see SetFlushHandler in
	// NewProcessorService and GracefulDegradation.triggerAsyncFlush. This
	// call site does not participate in flushing at all anymore.

	return nil
}

// dispatchAlert calls the alert dispatcher and guarantees it can never fail
// or block event processing (docs/plans/E2E_RECOVERY_PLAN.md P5-1 item 5):
// Dispatch itself only touches in-memory counters/config and a non-blocking
// notifier queue send (see alerts.BuildSender), so there is no network I/O
// on this call path to time out on — but a panic anywhere in a
// caller-supplied sender must still not be allowed to take down the event
// that triggered it.
func (s *ProcessorService) dispatchAlert(ctx context.Context, issueID, projectID, errorClass, message string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("alerts: recovered from panic dispatching alert for issue=%s project=%s: %v", issueID, projectID, r)
		}
	}()
	s.alerts.Dispatch(ctx, issueID, projectID, errorClass, message)
}
