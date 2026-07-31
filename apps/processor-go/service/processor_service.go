package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/degradation"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/indexer"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/procmetrics"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer is obtained from the otel API's global accessor rather than threaded in from main() — see
// procmetrics.meter's doc comment for why this is safe regardless of whether obs.Bootstrap has run
// yet. Child spans started here (deserialize/store/index/alert dispatch — OBSERVABILITY_PLAN.md W2)
// attach to whatever span main.go's subscriber handler already put in ctx (the consumer span, itself
// parented on the ingestor's producer span via the extracted traceparent).
var tracer trace.Tracer = otel.Tracer("processor-go")

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
// There is no in-memory buffering path anymore (see the degradation package's
// doc comment and docs/memory/BUGS.md B1): a down database always returns an
// error here so the event stays in JetStream, where D10's bounded retry with
// backoff (1s/5s/15s/30s/60s, MaxDeliver 5) owns recovery, and a DLQ entry
// preserves anything that outlasts that budget. This function must never call
// processEventInternal for an event that did not get StatusProcessed.
// ProcessEvent records MetricProcessDuration/MetricProcessEvents (OBSERVABILITY_PLAN.md §2/W2) around
// the whole call, classifying the outcome from the returned error AND the stored flag
// processEventInternal reports (docs/plans/IDEMPOTENCY_PLAN.md D-e): err != nil and nats.IsPermanent ->
// OutcomeDeadLettered (dead-lettered on THIS delivery, unconditionally, by the subscriber regardless of
// delivery count); err != nil otherwise -> OutcomeRetried; err == nil && !stored -> obs.OutcomeDuplicate
// (store.StoreEvent found the (issue_id, event_id) pair already stored and rolled back — a healthy
// no-op ACK, not a failure); err == nil && stored -> OutcomeStored. This classifier is the ONLY place
// that records a process outcome for a message — processEventInternal itself must never also record
// OutcomeDuplicate, or one message would mint two time series and the
// stored+duplicate+retried+deadlettered == deliveries invariant (W3) would stop holding.
//
// The retried-vs-deadlettered split is an approximation, not a certainty: whether NATS retries or
// actually dead-letters a non-permanent error depends on the message's delivery count, which lives
// entirely inside packages/shared-go/nats.Subscriber.handleMessage (private, and out of scope for this
// change — see OBSERVABILITY_PLAN.md's W2 brief). A transient error on the LAST allowed delivery is
// reported here as "retried" even though the subscriber will actually dead-letter it. This is the best
// classification available from this call's own return value without touching that package.
func (s *ProcessorService) ProcessEvent(ctx context.Context, data []byte) (err error) {
	start := time.Now()
	var stored bool
	defer func() {
		outcome := obs.OutcomeStored
		switch {
		case err != nil:
			if nats.IsPermanent(err) {
				outcome = obs.OutcomeDeadLettered
			} else {
				outcome = obs.OutcomeRetried
			}
		case !stored:
			outcome = obs.OutcomeDuplicate
		}
		procmetrics.RecordProcessed(ctx, outcome, time.Since(start))
	}()

	switch s.degradation.Evaluate(ctx, data) {
	case degradation.StatusProcessed:
		stored, err = s.processEventInternal(ctx, data)
		return err
	case degradation.StatusUnavailable:
		// Do NOT ACK. The database is down and this event has not been stored anywhere durable.
		// Returning an error keeps it in JetStream, where D10's bounded retry with backoff
		// (1s/5s/15s/30s/60s, MaxDeliver 5) owns recovery and a DLQ entry preserves anything that
		// outlasts the budget. An earlier version of this code ACKed here on the grounds that the
		// event "was buffered" in process memory — that silently destroyed events on a process
		// restart, trading a duplicate-delivery bug for a permanent-loss bug, which is strictly worse.
		return fmt.Errorf("database unavailable: event returned to NATS for bounded retry")
	default:
		return fmt.Errorf("unknown degradation status")
	}
}

// VerifyAuditLogTable confirms audit_logs is present and writable-shaped at boot.
//
// It used to INSERT a fixed all-zero-UUID row with action 'verification_test' — a permanent piece of fake
// audit data sitting in the audit trail, in the one table whose entire value is that everything in it
// really happened. ON CONFLICT DO NOTHING kept it to a single row rather than one per boot, which is
// precisely why nobody noticed it.
//
// A SELECT against the real column list proves the same things the INSERT did — the table exists and its
// columns match what this service expects — without writing anything. LIMIT 0 means no rows are fetched;
// Postgres still validates every identifier, so a renamed or dropped column fails here exactly as it
// would have before.
func (s *ProcessorService) VerifyAuditLogTable(ctx context.Context) error {
	const query = `SELECT id, action, resource_type, actor_id, metadata FROM audit_logs LIMIT 0`
	rows, err := s.db.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("audit_logs is not usable: %w", err)
	}
	rows.Close()
	return rows.Err()
}

// withSpan starts a child span named name on ctx, runs fn with the span-carrying context, records fn's
// error (if any) onto the span, and always ends the span before returning. Used throughout
// processEventInternal for the "meaningful stages" OBSERVABILITY_PLAN.md's W2 brief calls out:
// deserialize, store, index, alert dispatch — all children of whatever span is already in ctx (the
// consumer span main.go's subscriber handler opens after extracting the ingestor's producer span from
// the NATS traceparent header).
func withSpan(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	spanCtx, span := tracer.Start(ctx, name)
	defer span.End()
	if err := fn(spanCtx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// processEventInternal deserializes and stores one event, returning (stored, err) instead of a bare
// error (docs/plans/IDEMPOTENCY_PLAN.md D-e): stored distinguishes "persisted" from "this exact
// (issue_id, event_id) pair was already stored — store.StoreEvent rolled back and this is a healthy
// no-op ACK", which ProcessEvent's deferred classifier is the ONLY place allowed to turn into
// obs.OutcomeDuplicate (see that function's doc comment for why recording it here too would
// double-count).
func (s *ProcessorService) processEventInternal(ctx context.Context, data []byte) (bool, error) {
	var evt *event.ErrorEvent
	if err := withSpan(ctx, "processor.deserialize", func(ctx context.Context) error {
		var deserializeErr error
		evt, deserializeErr = event.Deserialize(data)
		return deserializeErr
	}); err != nil {
		// A deserialize failure (malformed proto, a required field missing)
		// is a property of these exact bytes: redelivering the same message
		// will fail identically forever, so it must not spend its whole
		// MaxDeliver budget being retried (VERIFIED_STATE.md S13).
		slog.ErrorContext(ctx, "Failed to deserialize event", slog.String("error", err.Error()))
		return false, nats.Permanent(err)
	}

	slog.InfoContext(ctx, "Processing event",
		slog.String("project", evt.ProjectKey), slog.String("project_id", evt.ProjectID),
		slog.String("error_class", evt.ErrorClass), slog.String("fingerprint", evt.Fingerprint),
		slog.String("release_version", evt.ReleaseVersion))

	var projectID string
	issue := &store.Issue{
		ID:          uuid.New().String(),
		Fingerprint: evt.Fingerprint,
		Message:     evt.Message,
		ErrorClass:  evt.ErrorClass,
		Status:      "unresolved",
		FirstSeen:   evt.Timestamp,
		LastSeen:    evt.Timestamp,
	}

	stacktraceJSON, _ := json.Marshal(evt.Stacktrace)
	metadataJSON, _ := json.Marshal(evt.Metadata)
	occ := &store.ErrorOccurrence{
		ID:             uuid.New().String(),
		Environment:    evt.Environment,
		Platform:       evt.Platform,
		ReleaseVersion: evt.ReleaseVersion,
		Stacktrace:     stacktraceJSON,
		Metadata:       metadataJSON,
		TraceID:        evt.TraceID,
		SpanID:         evt.SpanID,
		CreatedAt:      evt.Timestamp,
		EventID:        evt.EventID,
	}

	var stored bool
	storeErr := withSpan(ctx, "processor.store", func(ctx context.Context) error {
		var err error
		projectID, err = s.store.ResolveProjectID(ctx, evt.ProjectID, evt.ProjectKey)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to resolve project", slog.String("error", err.Error()))
			return classifyProjectLookupError(err)
		}
		issue.ProjectID = projectID

		// StoreEvent (docs/plans/IDEMPOTENCY_PLAN.md D-c) folds the issue upsert/regression
		// bookkeeping and the event_id-deduplicated occurrence insert into ONE transaction — this
		// replaces the former three-step UpsertIssueWithOutcome / GetIssueIDByFingerprint /
		// InsertOccurrence sequence, whose gap between the issue commit and the occurrence insert
		// was the exact S18 count-inflation window (IDEMPOTENCY_PLAN.md §1). Audit persistence,
		// alert dispatch, and search indexing all move to AFTER this call returns, gated on
		// stored==true (D-d) — see below.
		_, storeResult, err := s.store.StoreEvent(ctx, issue, occ, evt.ReleaseVersion)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to store event", slog.String("error", err.Error()))
			return classifyStoreError(err)
		}
		stored = storeResult
		return nil
	})
	if storeErr != nil {
		return false, storeErr
	}

	if !stored {
		// Duplicate: store.StoreEvent found (issue_id, event_id) already stored and rolled back
		// this delivery's issue/occurrence writes. This is a successful no-op — audit, alert
		// dispatch, and indexing must NOT run for it (D-d: they are gated on stored==true, because
		// each one reads state this delivery did not actually write). One structured log line is
		// D-e's whole visibility contract for this branch.
		//
		// The NATS delivery count is deliberately omitted here — a RECORDED DEVIATION from
		// IDEMPOTENCY_PLAN.md D-e, which asks for it (the plan's E2E_RECOVERY_PLAN.md P9-3 entry
		// records the deviation alongside the acceptance-text one). Getting it to this call site
		// would mean either changing nats.Subscriber's handler signature (14 call sites) or
		// changing ProcessEvent/processEventInternal's signature to carry headers all the way
		// down from main.go's Subscribe closure, for a single diagnostic log field. event_id and
		// issue_id alone are already enough to find the exact duplicate in error_occurrences,
		// and "redelivery vs client re-send" is distinguishable by whether the ingest-side POST
		// count moved.
		slog.InfoContext(ctx, "Duplicate event skipped: (issue_id, event_id) already stored",
			slog.String("event_id", occ.EventID), slog.String("issue_id", issue.ID))
		return false, nil
	}

	// stored == true from here on. Audit, alert dispatch, and indexing all run AFTER StoreEvent's
	// commit and ONLY for a delivery that actually wrote something (D-d) — order: commit (already
	// happened, above) -> audit -> alert -> index. All three are best-effort: PersistAuditLog
	// already swallows/logs its own failures (see its doc comment / AUDIT_PERSIST_FAILURE), and
	// dispatchAlert/IndexOccurrence are void/logged-not-propagated by design (see below).
	s.store.PersistAuditLog(ctx, &store.AuditLog{
		ID:           uuid.New().String(),
		Action:       "issue_upserted",
		ResourceType: "issue",
		ResourceID:   &issue.ID,
		ActorID:      "processor-go",
		Metadata:     []byte(fmt.Sprintf(`{"fingerprint": "%s", "project_id": "%s"}`, issue.Fingerprint, issue.ProjectID)),
	})
	s.store.PersistAuditLog(ctx, &store.AuditLog{
		ID:           uuid.New().String(),
		Action:       "occurrence_created",
		ResourceType: "error_occurrence",
		ResourceID:   &occ.ID,
		ActorID:      "processor-go",
		Metadata:     []byte(fmt.Sprintf(`{"issue_id": "%s", "environment": "%s"}`, occ.IssueID, occ.Environment)),
	})

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
	//
	// This span is best-effort and can never record an error: dispatchAlert (and, beneath it,
	// alerts.Dispatcher.Dispatch) is void by design — it swallows/logs its own failures internally
	// (queue-full, unroutable channel_config, panics in a caller-supplied sender, ...) so that a
	// broken notifier can never fail or block event processing (see dispatchAlert's doc comment).
	// There is no error left for this closure to propagate; changing that would mean changing
	// Dispatcher.Dispatch's exported signature, which is out of scope here. sentinel_alert_dispatch_total
	// (procmetrics.RecordAlertDispatch, including the dropped outcome) is the mechanism that actually
	// makes alert-dispatch failures observable, not this span's status.
	//
	// Dispatch now runs AFTER StoreEvent's commit (D-d), fixing the §1 double-feed: a redelivery that
	// used to feed the alert frequency counter for an event that was then rolled back at the
	// occurrence step can no longer happen, because dispatch does not run at all when stored==false.
	_ = withSpan(ctx, "processor.alert_dispatch", func(ctx context.Context) error {
		s.dispatchAlert(ctx, issue.ID, projectID, evt.ErrorClass, evt.Message)
		return nil
	})

	searchEntry := indexer.ExtractSearchFields(evt.Metadata)
	searchEntry.OccurrenceID = occ.ID
	if searchEntry.TraceID == "" {
		searchEntry.TraceID = evt.TraceID
	}
	if searchEntry.SpanID == "" {
		searchEntry.SpanID = evt.SpanID
	}

	_ = withSpan(ctx, "processor.index", func(ctx context.Context) error {
		if err := s.indexer.IndexOccurrence(ctx, searchEntry); err != nil {
			slog.ErrorContext(ctx, "Failed to index occurrence", slog.String("error", err.Error()))
			return err
		}
		return nil
	})

	return true, nil
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
			slog.ErrorContext(ctx, "alerts: recovered from panic dispatching alert",
				slog.String("issue_id", issueID), slog.String("project_id", projectID), slog.Any("panic", r))
		}
	}()
	s.alerts.Dispatch(ctx, issueID, projectID, errorClass, message)
}
