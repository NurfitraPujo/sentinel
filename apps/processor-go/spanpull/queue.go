package spanpull

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

const (
	MetricEnqueued = "sentinel_span_pull_enqueued_total" // outcome: queued | duplicate | throttled | not_allowed | not_validated | no_org | error
	// MetricFetches counts trace-store fetches. outcome: ok | not_found | too_large | error | gave_up. trigger: auto (queued
	// by MaybeEnqueue) | on_demand (a person or agent asked, via the dashboard or the agent API); an on-demand row is
	// processed exactly like an automatic one, the label is the only difference.
	MetricFetches = "sentinel_span_pull_fetches_total"
)

// Terminal outcomes the worker records on the queue row (span_pull_queue.outcome, 1725800000), so a reader can tell a
// finished pull that found nothing from one that has not run. "" means nothing has finished since the row was queued or
// re-armed.
const (
	OutcomeStored     = "stored"      // spans were written
	OutcomeNotFound   = "not_found"   // the trace store has no such trace (or an empty one)
	OutcomeTooLarge   = "too_large"   // over the read bound or refused by the store
	OutcomeNotAllowed = "not_allowed" // the organization left the allow-list
	OutcomeGaveUp     = "gave_up"     // retries exhausted
)

const (
	TriggerAuto     = "auto"
	TriggerOnDemand = "on_demand"
)

var (
	meter       = otel.Meter("processor-go")
	enqueuedCtr metric.Int64Counter
	fetchesCtr  metric.Int64Counter
)

func init() {
	enqueuedCtr, _ = meter.Int64Counter(MetricEnqueued, metric.WithDescription("Traces considered for a core span pull, by outcome"))
	fetchesCtr, _ = meter.Int64Counter(MetricFetches, metric.WithDescription("Trace-store fetches by outcome"))
}

func count(ctx context.Context, c metric.Int64Counter, outcome string, extra ...attribute.KeyValue) {
	if c != nil {
		c.Add(ctx, 1, metric.WithAttributes(append([]attribute.KeyValue{attribute.String("outcome", outcome)}, extra...)...))
	}
}

// Trigger is the MetricFetches label value for this job.
func (j Job) Trigger() string {
	if j.OnDemand {
		return TriggerOnDemand
	}
	return TriggerAuto
}

// DB is the slice of a pgx pool the queue uses (*pgxpool.Pool in main; a fake in unit tests).
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Queue is the Postgres side of D3: the enqueue the processor calls after an occurrence is stored, and the claim the
// worker takes.
type Queue struct {
	db  DB
	cfg Config
	now func() time.Time
}

func NewQueue(db DB, cfg Config) *Queue {
	return &Queue{db: db, cfg: cfg, now: time.Now}
}

// WithClock replaces the clock the due times and the issue window are computed from (tests).
func (q *Queue) WithClock(now func() time.Time) *Queue { q.now = now; return q }

// MaybeEnqueue queues traceID for a pull when the feature is on, traceID is canonical, projectID belongs to an
// allow-listed organization and issueID has not queued a pull within Config.IssueWindow. It NEVER returns an error and
// never blocks error ingest on anything slower than three primary-key statements: every failure is logged and counted
// (D3: "a fetch failure ... never blocks or fails error ingest"). It returns the MetricEnqueued outcome it counted, ""
// when the occurrence was not a candidate at all; the processor ignores it. The caller invokes it after the occurrence
// is committed.
func (q *Queue) MaybeEnqueue(ctx context.Context, projectID, issueID, traceID string) string {
	if q == nil || !q.cfg.Enabled() || !spanwire.ValidTraceID(traceID) || projectID == "" {
		return ""
	}
	outcome := q.enqueue(ctx, projectID, issueID, traceID)
	count(ctx, enqueuedCtr, outcome)
	return outcome
}

func (q *Queue) enqueue(ctx context.Context, projectID, issueID, traceID string) string {
	if !q.cfg.Validated() {
		return "not_validated"
	}
	var org *string
	err := q.db.QueryRow(ctx, `SELECT organization_id::text FROM projects WHERE id = $1::uuid`, projectID).Scan(&org)
	if err != nil || org == nil {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "span pull: organization lookup failed", slog.String("error", err.Error()))
			return "error"
		}
		return "no_org"
	}
	if !q.cfg.Allowed(*org) {
		return "not_allowed"
	}
	// The throttle comes after the allow-list, so an organization that is not allow-listed never writes a window row.
	if q.cfg.IssueWindow > 0 && issueID != "" {
		allowed, err := q.claimIssueWindow(ctx, issueID)
		if err != nil {
			slog.WarnContext(ctx, "span pull: issue window check failed", slog.String("error", err.Error()))
			return "error"
		}
		if !allowed {
			return "throttled"
		}
	}
	// trigger_project_id is the FIRST error's project, kept for debugging only (ON CONFLICT DO NOTHING never rewrites
	// it). It scopes nothing: pulled spans are governed by the configured core project (Config.Scopes), so which
	// project's error got here first makes no difference to who can see them.
	tag, err := q.db.Exec(ctx, `INSERT INTO span_pull_queue (org_id, trace_id, stage, next_fetch_at, trigger_project_id)
		VALUES ($1::uuid, $2, 0, $3, $4::uuid) ON CONFLICT (org_id, trace_id) DO NOTHING`,
		*org, traceID, q.now().Add(FirstFetchDelay), projectID)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "span pull: enqueue failed", slog.String("error", err.Error()))
		return "error"
	case tag.RowsAffected() == 0:
		return "duplicate"
	default:
		return "queued"
	}
}

// claimIssueWindow is the per-issue throttle: one statement that both decides and records, so two processor replicas
// handling occurrences of the same issue at the same moment cannot both pass it. The first occurrence inserts the row;
// a later one rewrites last_enqueued_at only when the window has elapsed, and the row comes back only when this call
// inserted or rewrote it. Concurrent callers serialize on the row, and ON CONFLICT DO UPDATE evaluates its WHERE
// against the latest committed version, so exactly one of them gets it back.
//
// The row is recorded BEFORE the queue insert, outside any transaction. When that insert then finds the trace already
// queued (ON CONFLICT, outcome duplicate) the window is still spent, which is fine: that trace is being pulled. When it
// errors, the issue queues nothing until the window elapses; that costs at most one window of pulls for one issue,
// and the database is failing anyway.
func (q *Queue) claimIssueWindow(ctx context.Context, issueID string) (bool, error) {
	now := q.now()
	var got string
	err := q.db.QueryRow(ctx, `INSERT INTO span_pull_issue_window (issue_id, last_enqueued_at) VALUES ($1::uuid, $2)
		ON CONFLICT (issue_id) DO UPDATE SET last_enqueued_at = EXCLUDED.last_enqueued_at
		 WHERE span_pull_issue_window.last_enqueued_at <= $3
		RETURNING issue_id::text`, issueID, now, now.Add(-q.cfg.IssueWindow)).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Job is one claimed queue row.
type Job struct {
	OrgID            string
	TraceID          string
	TriggerProjectID string // debugging only
	Stage            int
	// OnDemand is set when the row's last (re)arm came from a person or agent (span_pull_queue.on_demand). The worker
	// treats it like any other row; it only labels the metrics.
	OnDemand bool
	// Attempts is this job's attempt number AT THIS STAGE, counted when it was claimed: 1 on the first try. A worker
	// that crashes mid-fetch still spent the attempt, so a poison job cannot retry every lease forever.
	Attempts int
	Created  time.Time
}

// Claim takes up to limit due rows (stage < 2, next_fetch_at <= now), pushing each one's next_fetch_at ClaimLease
// ahead so no other worker takes it while this one fetches, and counting the attempt. FOR UPDATE SKIP LOCKED keeps
// replicas from blocking.
func (q *Queue) Claim(ctx context.Context, limit int) ([]Job, error) {
	rows, err := q.db.Query(ctx, `
		WITH due AS (
			SELECT org_id, trace_id FROM span_pull_queue
			 WHERE stage < 2 AND next_fetch_at <= $1
			 ORDER BY next_fetch_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE span_pull_queue q SET next_fetch_at = $3, attempts = q.attempts + 1
		  FROM due WHERE q.org_id = due.org_id AND q.trace_id = due.trace_id
		RETURNING q.org_id::text, q.trace_id, coalesce(q.trigger_project_id::text, ''), q.stage, q.attempts, q.created_at, q.on_demand`,
		q.now(), limit, q.now().Add(ClaimLease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		var stage int16
		if err := rows.Scan(&j.OrgID, &j.TraceID, &j.TriggerProjectID, &stage, &j.Attempts, &j.Created, &j.OnDemand); err != nil {
			return nil, err
		}
		j.Stage = int(stage)
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Advance records a finished (or given-up) stage: stage 0 -> 1 (the second fetch due SecondFetchDelay after the first
// error, never sooner than now), stage 1 -> 2 (done). It is conditional on the stage the job was CLAIMED at, so a
// worker whose lease expired and whose job was re-claimed and finished by another cannot advance it a second time
// (skipping the 10-minute fetch, or tripping the stage CHECK). It reports whether this call advanced the row.
func (q *Queue) Advance(ctx context.Context, j Job) (bool, error) { return q.AdvanceWith(ctx, j, "") }

// AdvanceWith is Advance that also records what the fetch ended in (an Outcome* constant) on the row, so the row of a
// finished (stage 2) job says why it holds no spans.
func (q *Queue) AdvanceWith(ctx context.Context, j Job, outcome string) (bool, error) {
	next := j.Created.Add(SecondFetchDelay)
	if now := q.now(); next.Before(now) {
		next = now
	}
	tag, err := q.db.Exec(ctx, `UPDATE span_pull_queue SET stage = stage + 1, attempts = 0, last_error = '', next_fetch_at = $4, outcome = $5
		WHERE org_id = $1::uuid AND trace_id = $2 AND stage = $3`, j.OrgID, j.TraceID, j.Stage, next, outcome)
	return tag.RowsAffected() > 0, err
}

// Fail records a failed fetch: the attempt was already counted at claim, so this only sets the retry time and the
// error. It is conditional on the claimed stage like Advance.
func (q *Queue) Fail(ctx context.Context, j Job, cause error) error {
	msg := truncUTF8(dropControls(strings.ToValidUTF8(cause.Error(), "?")), 500)
	_, err := q.db.Exec(ctx, `UPDATE span_pull_queue SET last_error = $4, next_fetch_at = $5
		WHERE org_id = $1::uuid AND trace_id = $2 AND stage = $3`, j.OrgID, j.TraceID, j.Stage, msg, q.now().Add(RetryDelay))
	return err
}

// Finish marks a job done without a successful fetch.
func (q *Queue) Finish(ctx context.Context, j Job) error { return q.FinishWith(ctx, j, "") }

// FinishWith is Finish that records why the job ended (OutcomeNotAllowed: its organization left the allow-list;
// OutcomeTooLarge: the trace cannot be pulled).
func (q *Queue) FinishWith(ctx context.Context, j Job, outcome string) error {
	_, err := q.db.Exec(ctx, `UPDATE span_pull_queue SET stage = 2, outcome = $3 WHERE org_id = $1::uuid AND trace_id = $2`, j.OrgID, j.TraceID, outcome)
	return err
}

// truncUTF8 cuts s to at most n bytes without splitting a rune (Postgres rejects a split one).
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
