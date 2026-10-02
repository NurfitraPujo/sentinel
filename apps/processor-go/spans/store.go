// Package spans stores sentinel-span-ingest's span rows: the SPANS consumer's write path (D2), the day-partition
// maintenance (D1) and the attribute masking and bounds (D7). The pull worker that produces `pulled` rows lives in
// apps/processor-go/spanpull and writes through the same Store.
package spans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

// Retention windows in whole UTC days. D1: spans are kept 14 days; partitions exist from yesterday through two days
// ahead (the cron keeps them there), so a span whose start_ts falls outside that window has no partition.
const (
	RetentionDays   = 14
	PartitionBack   = 1
	PartitionAhead  = 2
	insertBatchSize = 500

	pgCheckViolation = "23514" // also what Postgres reports for "no partition of relation "spans" found for row"
)

// Row is one span as it is written: the wire span after masking and bounds, plus the credential's tenant.
type Row struct {
	OrgID     string
	ProjectID string // "" → NULL (a pulled span has no project)
	// ScopeProjectID is the CORE project that governs a pulled span (D8, SPAN_PULL_SCOPES): it is shown only to a
	// viewer who can reach that project and whose environment grant on it permits the span's environment. "" → NULL
	// (mobile spans, which carry project_id).
	ScopeProjectID string
	Source         string
	spanwire.Span
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// valid reports whether a row can be written at all. One bad row must be dropped and counted, never allowed to fail
// the batch it travels in: a failed batch is redelivered and finally parked in the DLQ, and the rows beside it with it.
func (r Row) valid() bool {
	return (r.Source == spanwire.SourceMobile || r.Source == spanwire.SourcePulled) &&
		uuidRe.MatchString(r.OrgID) &&
		(r.ProjectID == "" || uuidRe.MatchString(r.ProjectID)) &&
		(r.ScopeProjectID == "" || uuidRe.MatchString(r.ScopeProjectID)) &&
		spanwire.ValidTraceID(r.TraceID) && spanwire.ValidSpanID(r.SpanID) &&
		!r.StartTS.IsZero() && r.DurationUS >= 0
}

// isNoPartition recognises Postgres's "no partition of relation "spans" found for row", which shares SQLSTATE 23514
// with every other CHECK violation; only this message means a day partition is missing.
func isNoPartition(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgCheckViolation && strings.Contains(pgErr.Message, "no partition of relation")
}

// isBadRow is a failure of ONE row's data (class 22 data exception, class 23 integrity violation) as opposed to the
// database being unavailable.
func isBadRow(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23"))
}

// Store writes the spans table.
type Store struct {
	db  *pgxpool.Pool
	now func() time.Time
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db, now: time.Now} }

// WithClock replaces the clock the partition window is computed from (tests).
func (s *Store) WithClock(now func() time.Time) *Store { s.now = now; return s }

// Window returns the half-open [from, to) range of start_ts the created partitions cover as of now, UTC day
// boundaries. A span outside it is dropped and counted (D1), never inserted into a default partition.
func Window(now time.Time) (from, to time.Time) {
	day := now.UTC().Truncate(24 * time.Hour)
	return day.AddDate(0, 0, -PartitionBack), day.AddDate(0, 0, PartitionAhead+1)
}

// InWindow reports whether ts falls in Window(now).
func InWindow(ts, now time.Time) bool {
	from, to := Window(now)
	return !ts.Before(from) && ts.Before(to)
}

// EnsurePartitions creates the day partitions of Window, skipping those that exist. The retention cron does the
// same hourly; the processor also calls it at start-up and from the SPANS consumer's housekeeping tick so a stalled
// cron cannot leave the window without a partition for today.
func (s *Store) EnsurePartitions(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT spans_ensure_partitions($1, $2)`, PartitionBack, PartitionAhead).Scan(&n)
	return n, err
}

// Insert writes rows with ON CONFLICT (org_id, trace_id, span_id, start_ts) DO NOTHING, in one transaction of
// batches of insertBatchSize, and returns how many rows were new. A row that cannot be valid is dropped and counted
// before the write; if a day partition is missing (the cron is behind) it is created once and the write retried;
// and if the batch is still refused for a reason in ONE row's data, the rows are written one by one so only the bad
// ones are dropped (counted `invalid`). Only a failure that is not about row data (the database being down) is
// returned, for the subscriber to redeliver.
func (s *Store) Insert(ctx context.Context, rows []Row) (int, error) {
	good := rows[:0:0]
	for _, r := range rows {
		if r.valid() {
			good = append(good, r)
		} else {
			RecordDropped(ctx, r.Source, DropInvalid, 1)
		}
	}
	if len(good) == 0 {
		return 0, nil
	}
	n, err := s.insertOnce(ctx, good)
	if isNoPartition(err) {
		if _, ensureErr := s.EnsurePartitions(ctx); ensureErr != nil {
			return 0, fmt.Errorf("spans: ensure partitions after a missing partition: %w", ensureErr)
		}
		n, err = s.insertOnce(ctx, good)
	}
	if err != nil && isBadRow(err) {
		return s.insertEach(ctx, good)
	}
	return n, err
}

// insertEach is the slow path after a batch was refused for a row's data: each row on its own, bad ones dropped.
func (s *Store) insertEach(ctx context.Context, rows []Row) (int, error) {
	inserted := 0
	for _, r := range rows {
		n, err := s.insertOnce(ctx, []Row{r})
		if isNoPartition(err) {
			// A span outside the window of existing partitions (the clock moved, the cron is behind): dropped like an
			// out-of-window span, not retried forever.
			RecordDropped(ctx, r.Source, DropOutOfWindow, 1)
			continue
		}
		if err != nil {
			if isBadRow(err) {
				RecordDropped(ctx, r.Source, DropInvalid, 1)
				continue
			}
			return inserted, err
		}
		inserted += n
	}
	return inserted, nil
}

const insertSQL = `INSERT INTO spans (org_id, project_id, environment, source, service_name, trace_id, span_id,
	parent_span_id, name, op, kind, start_ts, duration_us, status, attributes, scope_project_id)
	VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, NULLIF($16, '')::uuid)
	ON CONFLICT (org_id, trace_id, span_id, start_ts) DO NOTHING`

func (s *Store) insertOnce(ctx context.Context, rows []Row) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit

	inserted := 0
	for off := 0; off < len(rows); off += insertBatchSize {
		end := off + insertBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		batch := &pgx.Batch{}
		for _, r := range rows[off:end] {
			attrs := []byte("{}")
			if len(r.Attributes) > 0 {
				if attrs, err = json.Marshal(r.Attributes); err != nil {
					return 0, fmt.Errorf("spans: marshal attributes: %w", err)
				}
			}
			batch.Queue(insertSQL, r.OrgID, r.ProjectID, r.Environment, r.Source, r.ServiceName, r.TraceID, r.SpanID,
				r.ParentSpanID, r.Name, r.Op, r.Kind, r.StartTS, r.DurationUS, r.Status, attrs, r.ScopeProjectID)
		}
		br := tx.SendBatch(ctx, batch)
		for range rows[off:end] {
			tag, execErr := br.Exec()
			if execErr != nil {
				br.Close()
				return 0, execErr
			}
			inserted += int(tag.RowsAffected())
		}
		if err := br.Close(); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return inserted, nil
}

// ProjectInfo resolves a project's organization and name (the service name of a mobile span). found is false for a
// project that no longer exists.
func (s *Store) ProjectInfo(ctx context.Context, projectID string) (orgID, name string, found bool, err error) {
	var org *string
	err = s.db.QueryRow(ctx, `SELECT organization_id::text, name FROM projects WHERE id = $1::uuid`, projectID).Scan(&org, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if org != nil {
		orgID = *org
	}
	return orgID, name, true, nil
}
