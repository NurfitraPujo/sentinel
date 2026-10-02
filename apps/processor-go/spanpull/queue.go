package spanpull

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

const (
	MetricEnqueued = "sentinel_span_pull_enqueued_total" // outcome: queued | duplicate | not_allowed | no_org | error
	MetricFetches  = "sentinel_span_pull_fetches_total"  // outcome: ok | not_found | too_large | error | gave_up
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

func count(ctx context.Context, c metric.Int64Counter, outcome string) {
	if c != nil {
		c.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

// Queue is the Postgres side of D3: the enqueue the processor calls after an occurrence is stored, and the claim the
// worker takes.
type Queue struct {
	db  *pgxpool.Pool
	cfg Config
	now func() time.Time
}

func NewQueue(db *pgxpool.Pool, cfg Config) *Queue {
	return &Queue{db: db, cfg: cfg, now: time.Now}
}

// WithClock replaces the clock the due times are computed from (tests).
func (q *Queue) WithClock(now func() time.Time) *Queue { q.now = now; return q }

// MaybeEnqueue queues traceID for a pull when the feature is on, traceID is canonical and projectID belongs to an
// allow-listed organization. It NEVER returns an error and never blocks error ingest on anything slower than two
// primary-key reads: every failure is logged and counted (D3: "a fetch failure ... never blocks or fails error
// ingest"). The caller invokes it after the occurrence is committed.
func (q *Queue) MaybeEnqueue(ctx context.Context, projectID, traceID string) {
	if q == nil || !q.cfg.Enabled() || !spanwire.ValidTraceID(traceID) || projectID == "" {
		return
	}
	var org *string
	err := q.db.QueryRow(ctx, `SELECT organization_id::text FROM projects WHERE id = $1::uuid`, projectID).Scan(&org)
	if err != nil || org == nil {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "span pull: organization lookup failed", slog.String("error", err.Error()))
			count(ctx, enqueuedCtr, "error")
			return
		}
		count(ctx, enqueuedCtr, "no_org")
		return
	}
	if !q.cfg.Allowed(*org) {
		count(ctx, enqueuedCtr, "not_allowed")
		return
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
		count(ctx, enqueuedCtr, "error")
	case tag.RowsAffected() == 0:
		count(ctx, enqueuedCtr, "duplicate")
	default:
		count(ctx, enqueuedCtr, "queued")
	}
}

// Job is one claimed queue row.
type Job struct {
	OrgID            string
	TraceID          string
	TriggerProjectID string // debugging only
	Stage            int
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
		RETURNING q.org_id::text, q.trace_id, coalesce(q.trigger_project_id::text, ''), q.stage, q.attempts, q.created_at`,
		q.now(), limit, q.now().Add(ClaimLease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		var stage int16
		if err := rows.Scan(&j.OrgID, &j.TraceID, &j.TriggerProjectID, &stage, &j.Attempts, &j.Created); err != nil {
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
func (q *Queue) Advance(ctx context.Context, j Job) (bool, error) {
	next := j.Created.Add(SecondFetchDelay)
	if now := q.now(); next.Before(now) {
		next = now
	}
	tag, err := q.db.Exec(ctx, `UPDATE span_pull_queue SET stage = stage + 1, attempts = 0, last_error = '', next_fetch_at = $4
		WHERE org_id = $1::uuid AND trace_id = $2 AND stage = $3`, j.OrgID, j.TraceID, j.Stage, next)
	return tag.RowsAffected() > 0, err
}

// Fail records a failed fetch: the attempt was already counted at claim, so this only sets the retry time and the
// error. It is conditional on the claimed stage like Advance.
func (q *Queue) Fail(ctx context.Context, j Job, cause error) error {
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := q.db.Exec(ctx, `UPDATE span_pull_queue SET last_error = $4, next_fetch_at = $5
		WHERE org_id = $1::uuid AND trace_id = $2 AND stage = $3`, j.OrgID, j.TraceID, j.Stage, msg, q.now().Add(RetryDelay))
	return err
}

// Finish marks a job done without fetching (its organization left the allow-list).
func (q *Queue) Finish(ctx context.Context, j Job) error {
	_, err := q.db.Exec(ctx, `UPDATE span_pull_queue SET stage = 2 WHERE org_id = $1::uuid AND trace_id = $2`, j.OrgID, j.TraceID)
	return err
}
