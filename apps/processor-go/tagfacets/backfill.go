// Package tagfacets holds the one-off backfill of the issue_tag_values rollup (sentinel-issue-tag-facets
// spec §4): `processor backfill-tag-facets`, run once per environment by an operator as a Job created from
// the chart's suspended CronJob. It counts every occurrence stored before the rollup's triggers existed.
//
// The per-batch work is one SQL function the migration creates, issue_tag_values_backfill_batch(p_limit),
// so the SQL the proofs exercise is the SQL production runs. Each call is its own transaction: it locks
// the one-row state, refuses to run unless all five triggers exist and are enabled, takes the next
// p_limit unmarked occurrence ids above the committed cursor, locks their issues rows in id order, counts
// the ones still unmarked, and advances the cursor in the same transaction (spec §4.2-§4.4). A re-run
// resumes from the cursor; a finished run is a no-op.
package tagfacets

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAlreadyRunning is returned, without a batch having run, when another runner holds the session
// advisory lock. main exits 2 on it, so a mistaken second Job ends Failed rather than Complete.
var ErrAlreadyRunning = errors.New("tag facets backfill: another runner holds the advisory lock")

// advisoryKey is hashed by PostgreSQL (hashtext) on both the lock and the holder lookup.
const advisoryKey = "sentinel.tag_facets.backfill"

// Defaults, the values the chart passes (values.yaml tagFacetsBackfill).
const (
	DefaultBatch      = 200
	DefaultPause      = 200 * time.Millisecond
	DefaultDuty       = 0.25
	MaxBatch          = 5000
	defaultMaxRetries = 10
	defaultRetryDelay = time.Second
	progressEvery     = 10 * time.Second
	// defaultLockWait bounds any ONE issue lock wait inside a batch (SET LOCAL lock_timeout).
	defaultLockWait = 5 * time.Second
	// defaultLockBudget bounds the TOTAL time a batch may hold the issues it has already locked, across
	// every wait inside it (SET LOCAL statement_timeout on the whole issue_tag_values_backfill_batch
	// call). Contract §4.2: the backfill never holds a long lock on issues. lock_timeout alone only
	// bounds each individual wait; a batch that locks several hot issues in id order can wait up to
	// lock_timeout AGAIN for each one, so an ingest-style waiter on the FIRST (lowest-id) issue the batch
	// locked can be held for the sum of every wait after it, not just one. statement_timeout caps that
	// sum: past it Postgres cancels the whole statement (57014), the transaction rolls back and releases
	// every lock the batch held, and the caller retries with a smaller limit.
	defaultLockBudget = 5 * time.Second
	// minBatchOnTimeout is the floor a batch shrinks to after repeated 57014s.
	minBatchOnTimeout = 1
)

// Options configures one run. The zero value of a field means its default.
type Options struct {
	// Batch is the rows per batch transaction, 1-5000 (default 200).
	Batch int
	// Pause is the floor of the sleep between batches (default 200ms).
	Pause time.Duration
	// Duty caps the fraction of wall-clock time a batch holds its issues' rows, in (0, 1] (default
	// 0.25): after a batch of elapsed time t the run sleeps max(Pause, t × (1 − Duty) / Duty).
	Duty float64
	// Logger receives the tag_facets.backfill.* lines (default slog.Default()).
	Logger *slog.Logger
	// MaxRetries bounds consecutive retries of a batch that failed with 40001, 40P01, 55P03 or 57014
	// (default 10). RetryDelay is the sleep before each retry (default 1s).
	MaxRetries int
	RetryDelay time.Duration
	// LockWait bounds any ONE issue lock wait inside a batch, SET LOCAL lock_timeout (default 5s).
	LockWait time.Duration
	// LockBudget bounds the TOTAL time a single batch call may hold the issues it has already locked,
	// SET LOCAL statement_timeout on the whole batch call (default 5s; contract §4.2). Past it the batch
	// is cancelled (57014), rolled back, and retried at half the limit, down to a floor of 1.
	LockBudget time.Duration
	// ProgressEvery is the minimum interval between tag_facets.backfill.progress log lines (default 10s).
	// Tests shrink it to observe the line without waiting out the real default.
	ProgressEvery time.Duration
	// Sleep replaces the timer between batches and before retries; it returns early with ctx's error
	// when ctx ends. Tests use it to observe or cut a run short. Default: a timer honouring ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Result is what a run did.
type Result struct {
	// Batches is the number of batch calls that committed and counted rows in this run.
	Batches int
	// RowsCounted is the state row's running total at the end (every run's rows, not only this one's).
	RowsCounted int64
	CursorID    string
	Done        bool
	// AlreadyDone is true when the run found the backfill finished and wrote nothing.
	AlreadyDone bool
	Elapsed     time.Duration
}

func (o Options) withDefaults() (Options, error) {
	if o.Batch == 0 {
		o.Batch = DefaultBatch
	}
	if o.Batch < 1 || o.Batch > MaxBatch {
		return o, fmt.Errorf("tag facets backfill: batch %d outside 1-%d", o.Batch, MaxBatch)
	}
	if o.Pause == 0 {
		o.Pause = DefaultPause
	}
	if o.Pause < 0 {
		o.Pause = 0
	}
	if o.Duty == 0 {
		o.Duty = DefaultDuty
	}
	if o.Duty <= 0 || o.Duty > 1 || math.IsNaN(o.Duty) {
		return o, fmt.Errorf("tag facets backfill: duty %v outside (0, 1]", o.Duty)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = defaultMaxRetries
	}
	if o.RetryDelay == 0 {
		o.RetryDelay = defaultRetryDelay
	}
	if o.LockWait == 0 {
		o.LockWait = defaultLockWait
	}
	if o.LockBudget == 0 {
		o.LockBudget = defaultLockBudget
	}
	if o.ProgressEvery == 0 {
		o.ProgressEvery = progressEvery
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	return o, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// PauseAfter is the sleep after a batch that took elapsed: max(pause, elapsed × (1 − duty) / duty), so
// the fraction of time a batch holds its issues stays ≤ duty whatever a batch costs (spec §4.2).
func PauseAfter(elapsed, pause time.Duration, duty float64) time.Duration {
	d := time.Duration(float64(elapsed) * (1 - duty) / duty)
	if d < pause {
		return pause
	}
	return d
}

type state struct {
	CursorID    *string
	Done        bool
	RowsCounted int64
}

func readState(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (state, error) {
	var s state
	err := q.QueryRow(ctx, `SELECT cursor_id::text, done, rows_counted FROM issue_tag_values_backfill`).
		Scan(&s.CursorID, &s.Done, &s.RowsCounted)
	return s, err
}

func (s state) cursor() string {
	if s.CursorID == nil {
		return ""
	}
	return *s.CursorID
}

// Run backfills until done, the context ends, or a batch fails for good. It holds one pooled connection
// for the whole run: the session advisory lock lives on it, and every batch runs on it.
func Run(ctx context.Context, pool *pgxpool.Pool, opts Options) (Result, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return Result{}, err
	}
	log := opts.Logger
	started := time.Now()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("tag facets backfill: acquire connection: %w", err)
	}
	defer conn.Release()

	// One runner at a time (spec §4.1). Exactness does not need it (runners serialise on the state row),
	// but each runner paces itself, so two would hold hot issues up to twice the duty cycle.
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, advisoryKey).Scan(&locked); err != nil {
		return Result{}, fmt.Errorf("tag facets backfill: advisory lock: %w", err)
	}
	if !locked {
		var holder *int64
		_ = conn.QueryRow(ctx, `
			SELECT pid::bigint FROM pg_locks
			 WHERE locktype = 'advisory' AND granted AND objsubid = 1
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			   AND ((classid::bigint << 32) | objid::bigint) = hashtext($1)::bigint
			 LIMIT 1`, advisoryKey).Scan(&holder)
		attrs := []any{slog.String("event", "tag_facets.backfill.already_running")}
		if holder != nil {
			attrs = append(attrs, slog.Int64("pid", *holder))
		}
		log.WarnContext(ctx, "tag facets backfill: another runner holds the lock; exiting", attrs...)
		return Result{}, ErrAlreadyRunning
	}
	defer func() {
		// Best effort: the lock goes with the session anyway, and conn.Release returns it to the pool.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, advisoryKey)
	}()

	st, err := readState(ctx, conn)
	if err != nil {
		return Result{}, fmt.Errorf("tag facets backfill: read state: %w", err)
	}
	log.InfoContext(ctx, "tag facets backfill: start",
		slog.String("event", "tag_facets.backfill.start"),
		slog.Int("batch", opts.Batch), slog.String("pause", opts.Pause.String()), slog.Float64("duty", opts.Duty),
		slog.String("cursor_id", st.cursor()), slog.Int64("rows_counted", st.RowsCounted), slog.Bool("done", st.Done))
	if st.Done {
		log.InfoContext(ctx, "tag facets backfill: already done; nothing to do",
			slog.String("event", "tag_facets.backfill.already_done"),
			slog.Int64("rows_counted", st.RowsCounted), slog.String("cursor_id", st.cursor()))
		return Result{RowsCounted: st.RowsCounted, CursorID: st.cursor(), Done: true, AlreadyDone: true,
			Elapsed: time.Since(started)}, nil
	}

	res := Result{RowsCounted: st.RowsCounted, CursorID: st.cursor()}
	var (
		retries       int
		limit         = opts.Batch
		lastProgress  = time.Now()
		intervalMs    []float64
		intervalRows  int64
		intervalStart = time.Now()
	)
	for {
		if err := ctx.Err(); err != nil {
			res.Elapsed = time.Since(started)
			return res, fmt.Errorf("tag facets backfill: stopped at cursor %s: %w", res.CursorID, err)
		}
		t0 := time.Now()
		counted, cursor, done, err := runBatch(ctx, conn, limit, opts.LockWait, opts.LockBudget)
		elapsed := time.Since(t0)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "57014" && retries < opts.MaxRetries {
				// statement_timeout: this batch's total lock-hold budget (contract §4.2) was exceeded.
				// The transaction rolled back and released every lock it held; retry at half the limit,
				// down to a floor of 1, so the next attempt holds fewer issues for less time.
				retries++
				if limit > minBatchOnTimeout {
					limit = limit / 2
					if limit < minBatchOnTimeout {
						limit = minBatchOnTimeout
					}
				}
				log.WarnContext(ctx, "tag facets backfill: batch exceeded its lock budget; retrying smaller",
					slog.String("event", "tag_facets.backfill.lock_budget_exceeded"), slog.String("code", pgErr.Code),
					slog.Int("attempt", retries), slog.Int("limit", limit), slog.String("error", pgErr.Message))
				if err := opts.Sleep(ctx, opts.RetryDelay); err != nil {
					res.Elapsed = time.Since(started)
					return res, fmt.Errorf("tag facets backfill: stopped at cursor %s: %w", res.CursorID, err)
				}
				continue
			}
			if errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01" || pgErr.Code == "55P03") &&
				retries < opts.MaxRetries {
				retries++
				log.WarnContext(ctx, "tag facets backfill: batch rolled back; retrying",
					slog.String("event", "tag_facets.backfill.retry"), slog.String("code", pgErr.Code),
					slog.Int("attempt", retries), slog.String("error", pgErr.Message))
				if err := opts.Sleep(ctx, opts.RetryDelay); err != nil {
					res.Elapsed = time.Since(started)
					return res, fmt.Errorf("tag facets backfill: stopped at cursor %s: %w", res.CursorID, err)
				}
				continue
			}
			res.Elapsed = time.Since(started)
			return res, fmt.Errorf("tag facets backfill: batch at cursor %s: %w", res.CursorID, err)
		}
		retries = 0
		if limit < opts.Batch {
			// A successful batch is evidence the transient hold is gone; grow back toward opts.Batch so
			// one lock-budget timeout does not permanently shrink the rest of the Job (tfr2's fix).
			limit *= 2
			if limit > opts.Batch {
				limit = opts.Batch
			}
		}
		if done {
			final, err := readState(ctx, conn)
			if err != nil {
				return res, fmt.Errorf("tag facets backfill: read final state: %w", err)
			}
			res.RowsCounted, res.CursorID, res.Done, res.Elapsed = final.RowsCounted, final.cursor(), true, time.Since(started)
			log.InfoContext(ctx, "tag facets backfill: done",
				slog.String("event", "tag_facets.backfill.done"),
				slog.Int64("rows_counted", res.RowsCounted), slog.Int("batches", res.Batches),
				slog.String("elapsed", res.Elapsed.Round(time.Millisecond).String()))
			return res, nil
		}
		res.Batches++
		res.RowsCounted += int64(counted)
		res.CursorID = cursor
		intervalMs = append(intervalMs, float64(elapsed.Microseconds())/1000)
		intervalRows += int64(counted)
		if time.Since(lastProgress) >= opts.ProgressEvery {
			p50, pmax := percentile(intervalMs, 0.5), percentile(intervalMs, 1)
			secs := time.Since(intervalStart).Seconds()
			log.InfoContext(ctx, "tag facets backfill: progress",
				slog.String("event", "tag_facets.backfill.progress"),
				slog.Int("batches", res.Batches), slog.Int64("rows_counted", res.RowsCounted),
				slog.String("cursor_id", res.CursorID), slog.Float64("progress_pct", ProgressPct(res.CursorID)),
				slog.Float64("rows_per_sec", float64(intervalRows)/secs),
				slog.Float64("batch_ms_p50", p50), slog.Float64("batch_ms_max", pmax))
			lastProgress, intervalStart, intervalMs, intervalRows = time.Now(), time.Now(), intervalMs[:0], 0
		}
		if err := opts.Sleep(ctx, PauseAfter(elapsed, opts.Pause, opts.Duty)); err != nil {
			res.Elapsed = time.Since(started)
			return res, fmt.Errorf("tag facets backfill: stopped at cursor %s: %w", res.CursorID, err)
		}
	}
}

// runBatch is one issue_tag_values_backfill_batch call in its own transaction: each individual issue lock
// wait bounded by lockWait (lock_timeout), and the batch's total hold time — across every wait inside it —
// bounded by lockBudget (statement_timeout), so an ingest-style waiter on the first issue the batch locked
// can never wait longer than lockBudget, no matter how many later issues in the batch are also held.
func runBatch(ctx context.Context, conn *pgxpool.Conn, limit int, lockWait, lockBudget time.Duration) (counted int, cursor string, done bool, err error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, lockWait.Milliseconds())); err != nil {
		return 0, "", false, err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = '%dms'`, lockBudget.Milliseconds())); err != nil {
		return 0, "", false, err
	}
	var c *string
	if err := tx.QueryRow(ctx, `SELECT counted, cursor_id::text, done FROM issue_tag_values_backfill_batch($1)`, limit).
		Scan(&counted, &c, &done); err != nil {
		return 0, "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", false, err
	}
	if c != nil {
		cursor = *c
	}
	return counted, cursor, done, nil
}

// ProgressPct estimates the fraction done from the cursor alone: occurrence ids are uniformly random v4
// UUIDs, so the cursor's first 64 bits over 2^64 is an unbiased estimate, with no count(*) of the table.
func ProgressPct(cursor string) float64 {
	b, err := hex.DecodeString(strings.ReplaceAll(cursor, "-", ""))
	if err != nil || len(b) < 8 {
		return 0
	}
	return math.Round(float64(binary.BigEndian.Uint64(b[:8]))/math.Pow(2, 64)*10000) / 100
}

func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}
