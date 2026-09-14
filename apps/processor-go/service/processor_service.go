package service

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/jackc/pgx/v5/pgconn"
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
		ID: uuid.New().String(),
		// sentinel-project-environments §4.9: part of the issue's identity, not just the
		// occurrence's. Taken from the event (§4.8) — the registry is never consulted here.
		Environment: evt.Environment,
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
		// sentinel-affected-users §4.3: the pre-normalisation, decision-5-bounded identifiers and the
		// reason each was dropped when it was — read by event.Normalize (decision 3), never from
		// evt.Metadata, which by now holds "<UUID>" where a UUID was. StoreEvent writes the side
		// tables from the ids and counts the reasons after commit.
		AffectedUserID:           evt.AffectedUserID,
		AffectedTenantID:         evt.AffectedTenantID,
		AffectedUserSkipReason:   evt.AffectedUserSkipReason,
		AffectedTenantSkipReason: evt.AffectedTenantSkipReason,
	}

	var stored bool
	// outcome is the class of the issue row's status AFTER the write (store.IssueOutcome, decision
	// 29). It used to be discarded here with `_`; sentinel-releases-snooze §4.3 / decision 16 makes
	// the service read it, for exactly one purpose — the dispatchAlert gate below.
	var outcome store.IssueOutcome
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
		storeOutcome, storeResult, err := s.store.StoreEvent(ctx, issue, occ, evt.ReleaseVersion)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to store event", slog.String("error", err.Error()))
			return classifyStoreError(err)
		}
		stored = storeResult
		outcome = storeOutcome
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

	// sentinel-releases-snooze §4.3 "After commit" (decisions 15, 41): the `next` rewrite, only when
	// step 4c's RETURNING inserted was true — this delivery is the FIRST sighting of (project, R), so
	// every resolved issue of the project waiting on `next` with resolved_at < first_seen now knows its
	// version. Here — after the audit rows, before alert dispatch and indexing — because it is one
	// more best-effort post-commit call of exactly their kind: its own short transaction, never inside
	// StoreEvent's (it wants nothing an ingest holds after its issues row, so it can never deadlock
	// with one), its failure logged and counted and never returned. occ.ReleaseInserted is written
	// back by StoreEvent only after tx.Commit, so a rolled-back first sighting never reaches this.
	if occ.ReleaseInserted {
		_ = withSpan(ctx, "processor.release_next_rewrite", func(ctx context.Context) error {
			s.rewriteNextIssues(ctx, projectID, occ.ReleaseVersion, occ.CreatedAt)
			return nil
		})
	}

	// Dispatch on EVERY occurrence, not only new/regressed issues — EXCEPT an occurrence whose issue
	// is `ignored` after the write (sentinel-releases-snooze decision 16, the gate below).
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
	//
	// The gate (sentinel-releases-snooze §4.3 "The outcome", decisions 16, 17, 29): dispatch is
	// skipped iff the outcome is IssueOutcomeIgnored — the row's status after the write is `ignored`,
	// a snooze still running or a plain indefinite ignore. The occurrence is stored, counted and
	// bucketed (StoreEvent already did all of that); it simply never reaches the dispatcher's
	// threshold counters, which is what "ignore" means. Every other outcome dispatches exactly as
	// before — including IssueOutcomeUnsnoozed, the occurrence that ENDED a snooze: its row is
	// `unresolved` now and it goes through the threshold normally, neither bypassed nor withheld
	// (decision 17 forbids an immediate alert, not the ordinary one). `!= Ignored` rather than an
	// allow-list of the other four, so a sixth outcome added later dispatches by default — silence is
	// the failure mode this gate must never widen into by accident.
	if outcome != store.IssueOutcomeIgnored {
		_ = withSpan(ctx, "processor.alert_dispatch", func(ctx context.Context) error {
			// issue.Environment, not occ.Environment: they are the same value on this path (both come
			// from the event), but the alert scope in §4.7 is a decision about the ISSUE's environment,
			// which decision 7 made part of its identity.
			s.dispatchAlert(ctx, issue.ID, projectID, issue.Environment, evt.ErrorClass, evt.Message)
			return nil
		})
	} else {
		slog.InfoContext(ctx, "Alert dispatch skipped: issue is ignored after the write (decision 16)",
			slog.String("issue_id", issue.ID), slog.String("project_id", projectID))
	}

	searchEntry := indexer.ExtractSearchFields(evt.Metadata)
	searchEntry.OccurrenceID = occ.ID
	// sentinel-affected-users §4.3 (last paragraph; decisions 13 and 19): error_search_index receives
	// the SAME raw ids the side tables got. ExtractSearchFields just read them out of the
	// post-normalisation map, where a UUID is "<UUID>" and an email "<EMAIL>" — so /search by user id
	// has never matched a UUID-, numeric- or email-keyed producer. Overwrite both with the event's
	// pre-normalisation, decision-5-bounded fields: "" when the value was unusable, which
	// IndexOccurrence's nullString already writes as NULL — so a 300-byte id is ABSENT from the index
	// instead of failing this after-commit VARCHAR(255) insert as it would have. ExtractSearchFields,
	// its key list, the columns and every reader are untouched; only these two values differ.
	searchEntry.UserID = evt.AffectedUserID
	searchEntry.TenantID = evt.AffectedTenantID
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

// releaseNextRewriteQuery is sentinel-releases-snooze §4.3's "After commit" statement plus decision
// 49's NOT EXISTS clause: $1 = project_id, $2 = the newly inserted release's version, $3 = its
// first_seen (occ.CreatedAt — the value step 4c's INSERT arm wrote). The comparison is STRICT
// (`first_seen > resolved_at`), and a row with resolved_at IS NULL never matches because `NULL < x` is
// not true — decision 27's rows are left alone, exactly as the dashboard's hourly catch-up leaves
// them. Project-wide across environments (decision 41). `idx_issues_project_resolved_next` (a
// partial index on project_id WHERE resolved_in_version = 'next') makes this an index scan over
// exactly the waiting issues.
//
// The NOT EXISTS (decision 49, `fanout-earliest-newer`) makes the fan-out write ONLY the earliest
// release newer than the row's resolved_at: a `next` issue for which some OTHER release of the
// project was already first seen strictly between its resolved_at and this release's first_seen is
// skipped — that earlier release's own fan-out failed (the pod died, or decision 42's accepted 40P01)
// and the row belongs to the hourly catch-up, which picks earliest-newer by construction. Without it
// the following release would write a version LATER than the truth, and an occurrence of the issue
// in the earlier release would then be judged rowR.first_seen < rowX.first_seen → not a regression
// (decision 13) — the issue would stay resolved on a release that still throws it. §8's "the
// following new release can never write a later version than the truth" is true only with this
// clause. `r.first_seen < $3` is strict so the release being fanned out never excludes itself.
const releaseNextRewriteQuery = `
	UPDATE issues SET resolved_in_version = $2
	WHERE project_id = $1 AND status = 'resolved' AND resolved_in_version = 'next'
	  AND resolved_at < $3
	  AND NOT EXISTS (
	    SELECT 1 FROM releases r
	    WHERE r.project_id = $1 AND r.first_seen > issues.resolved_at AND r.first_seen < $3
	  )
`

// rewriteNextIssues runs the post-commit `next` rewrite for a release this delivery was the first to
// see (sentinel-releases-snooze §4.3 "After commit", decisions 8, 15, 42). It is best-effort in the
// same sense PersistAuditLog and dispatchAlert are: it never returns an error, never panics out, and
// never affects the Ack — the event is stored already, and a `next` issue this call fails to rewrite
// keeps reading `next` until the dashboard's hourly catch-up (cleanupRetainedData's last statement)
// rewrites it to the same earliest-newer release this call would have. The window is one hour, and
// it is the release note's to state, not this function's to close.
//
// Its OWN short transaction — one autocommit UPDATE on the pool, never StoreEvent's transaction:
// inside it the fan-out would take every waiting issues row of the project under the ingest lock
// order and hold them to commit; outside it holds those rows for one statement and wants nothing an
// ingest transaction holds AFTER its issues row (it takes no releases row), so ingest-vs-rewrite has
// no cycle. What CAN deadlock is a batchUpdateIssues call over a set containing this project's
// `next` issues in the same second (three multi-row writers on issues with unspecified relative
// order, decision 42): Postgres kills one side with 40P01, and if it is this one the log line below
// names that SQLSTATE so the cause is legible without a metric label. No activity row per rewritten
// issue (decision 8): the resolution did not change, its version became known.
func (s *ProcessorService) rewriteNextIssues(ctx context.Context, projectID, version string, firstSeen time.Time) {
	tag, err := s.db.Exec(ctx, releaseNextRewriteQuery, projectID, version, firstSeen)
	if err != nil {
		// The SQLSTATE is the one field that tells a 40P01 (decision 42's accepted deadlock, self-
		// healing) from a 42703/42P01 (a migration not applied — §12's rollout-order failure) from a
		// connection failure (no *pgconn.PgError at all, "" here). It goes on the log line, never on
		// the counter: the counter is unlabelled by contract (decision 30).
		sqlstate := ""
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			sqlstate = pgErr.Code
		}
		slog.ErrorContext(ctx, "release next rewrite failed; the event is stored and the hourly catch-up will rewrite (contract §4.3)",
			slog.String("project_id", projectID),
			slog.String("release_version", version),
			slog.String("sqlstate", sqlstate),
			slog.String("error", err.Error()))
		procmetrics.RecordReleaseNextRewriteFailure(ctx)
		return
	}
	slog.InfoContext(ctx, "release next rewrite committed",
		slog.String("project_id", projectID),
		slog.String("release_version", version),
		slog.Int64("issues_rewritten", tag.RowsAffected()))
}

// dispatchAlert calls the alert dispatcher and guarantees it can never fail
// or block event processing (docs/plans/E2E_RECOVERY_PLAN.md P5-1 item 5):
// Dispatch itself only touches in-memory counters/config and a non-blocking
// notifier queue send (see alerts.BuildSender), so there is no network I/O
// on this call path to time out on — but a panic anywhere in a
// caller-supplied sender must still not be allowed to take down the event
// that triggered it.
func (s *ProcessorService) dispatchAlert(ctx context.Context, issueID, projectID, environment, errorClass, message string) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "alerts: recovered from panic dispatching alert",
				slog.String("issue_id", issueID), slog.String("project_id", projectID), slog.Any("panic", r))
		}
	}()
	s.alerts.Dispatch(ctx, issueID, projectID, environment, errorClass, message)
}
