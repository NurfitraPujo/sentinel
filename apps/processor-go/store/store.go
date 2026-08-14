package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// occurrenceEventMinInterval is the R2 throttle window for 'occurrence_burst' issue_activity
// rows (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7a): at most one burst event per issue
// per this interval, bounding org-wide event volume to active_issues/interval regardless of
// traffic. Configurable via OCCURRENCE_EVENT_MIN_INTERVAL_SECONDS (default 3600 = 1h); read once
// at package init since it never changes for the life of the process.
var occurrenceEventMinInterval = readOccurrenceEventMinInterval()

func readOccurrenceEventMinInterval() time.Duration {
	const defaultSeconds = 3600
	seconds := defaultSeconds
	if s := os.Getenv("OCCURRENCE_EVENT_MIN_INTERVAL_SECONDS"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			seconds = v
		}
	}
	return time.Duration(seconds) * time.Second
}

// QueryStore defines the "Read" side of the Issue store.
type QueryStore interface {
	GetProjectByKey(ctx context.Context, projectKey string) (string, error)
	// ResolveProjectID resolves the tenant project for an incoming event.
	// See the method doc on pgStore.ResolveProjectID for the full rationale.
	ResolveProjectID(ctx context.Context, projectID, projectKey string) (string, error)
	GetIssueIDByFingerprint(ctx context.Context, projectID, fingerprint string) (string, error)
}

// CommandStore defines the "Write" side of the Issue store.
type CommandStore interface {
	UpsertIssue(ctx context.Context, issue *Issue, releaseVersion string) error
	// UpsertIssueWithOutcome behaves exactly like UpsertIssue but additionally
	// reports whether the issue was newly created, was a regression, or
	// already existed unchanged. This is the signal the alert dispatcher
	// needs (docs/plans/E2E_RECOVERY_PLAN.md P5-1 / VERIFIED_STATE.md S8):
	// alerts must fire for new issues and regressions, not every occurrence.
	// UpsertIssue itself keeps its original single-error signature (it is
	// exercised directly by tests/integration/processor_store_test.go) and
	// is now a thin wrapper around this method.
	//
	// Kept for tests/integration/processor_store_test.go (W3's file, ~13 call
	// sites); the service package no longer calls it — see StoreEvent below.
	UpsertIssueWithOutcome(ctx context.Context, issue *Issue, releaseVersion string) (IssueOutcome, error)
	// InsertOccurrence is the pre-idempotency bare-exec insert (no event_id, no dedup, no shared
	// transaction with the issue upsert). Kept for tests/integration/processor_store_test.go; the
	// service package no longer calls it — see StoreEvent below.
	InsertOccurrence(ctx context.Context, occ *ErrorOccurrence) error
	PersistAuditLog(ctx context.Context, log *AuditLog) error
	// StoreEvent is the atomic, duplicate-aware write path (docs/plans/IDEMPOTENCY_PLAN.md D-c): one
	// transaction folds the issue upsert/regression bookkeeping and the event_id-deduplicated
	// occurrence insert together, so a partial-failure redelivery (NAK after the issue commits but
	// before the occurrence lands) rolls back BOTH instead of double-counting issues.count (S18 — the
	// defect W1/W2 close). See the method doc below for the full contract.
	StoreEvent(ctx context.Context, issue *Issue, occ *ErrorOccurrence, releaseVersion string) (outcome IssueOutcome, stored bool, err error)
}

// IssueOutcome reports what UpsertIssueWithOutcome actually did.
type IssueOutcome int

const (
	// IssueOutcomeExisting means a non-resolved issue already existed for
	// this fingerprint; this occurrence is just another duplicate.
	IssueOutcomeExisting IssueOutcome = iota
	// IssueOutcomeNew means no issue existed for this fingerprint before
	// this call.
	IssueOutcomeNew
	// IssueOutcomeRegressed means a previously resolved issue reappeared.
	IssueOutcomeRegressed
)

// issuesUpsertConflictPredicate scopes the issues ON CONFLICT (project_id, fingerprint) DO
// UPDATE to processor-owned rows only. The processor never writes issue_type, but a manual
// (issue_type = 'user_report') issue could theoretically collide on (project_id, fingerprint);
// without this predicate the processor's upsert would bump last_seen/count on that manual issue
// and Dispatcher.Dispatch would fire for it. See R13, docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md §10.
const issuesUpsertConflictPredicate = "issues.fingerprint = EXCLUDED.fingerprint AND issues.issue_type = 'system_error'"

// IssueStore combines both Read and Write operations for the Processor.
type IssueStore interface {
	QueryStore
	CommandStore
}

type pgStore struct {
	db *pgxpool.Pool
}

// NewStore returns a concrete implementation of IssueStore using PostgreSQL.
func NewStore(db *pgxpool.Pool) IssueStore {
	return &pgStore{db: db}
}

type Issue struct {
	ID                string
	ProjectID         string
	Fingerprint       string
	Message           string
	ErrorClass        string
	Status            string
	RegressionStatus  string
	ResolvedInVersion string
	FirstSeen         time.Time
	LastSeen          time.Time
	Count             int64
}

type ErrorOccurrence struct {
	ID             string
	IssueID        string
	Environment    string
	Platform       string
	ReleaseVersion string
	Stacktrace     json.RawMessage
	Metadata       json.RawMessage
	TraceID        string
	SpanID         string
	CreatedAt      time.Time
	// EventID is the idempotency key (docs/plans/IDEMPOTENCY_PLAN.md D-a/D-b), copied from the wire by
	// event.Deserialize BEFORE Normalize (D-h) and bounds-clamped to "" for anything that would not fit
	// VARCHAR(64) or carries a control character (D-g). "" here means "no key" — StoreEvent maps it to
	// SQL NULL via NULLIF at the insert site, never storing the empty string (D-b's tripwire CHECK
	// constraint rejects '' outright). Never log this value verbatim in a metric label (D15); it is a
	// client-supplied string that can appear an unbounded number of times.
	EventID string
}

// isRegressionVersion compares incoming releaseVersion against resolvedInVersion.
func isRegressionVersion(releaseVersion, resolvedInVersion string) bool {
	if releaseVersion == "" || resolvedInVersion == "" {
		return true
	}

	cleanRel := strings.TrimPrefix(releaseVersion, "v")
	cleanRes := strings.TrimPrefix(resolvedInVersion, "v")

	relParts := strings.Split(cleanRel, ".")
	resParts := strings.Split(cleanRes, ".")

	maxLen := len(relParts)
	if len(resParts) > maxLen {
		maxLen = len(resParts)
	}

	for i := 0; i < maxLen; i++ {
		relNum := 0
		resNum := 0
		if i < len(relParts) {
			relNum, _ = strconv.Atoi(strings.TrimRight(relParts[i], "abcdefghijklmnopqrstuvwxyz-"))
		}
		if i < len(resParts) {
			resNum, _ = strconv.Atoi(strings.TrimRight(resParts[i], "abcdefghijklmnopqrstuvwxyz-"))
		}
		if relNum > resNum {
			return true
		}
		if relNum < resNum {
			return false
		}
	}
	return true
}

// UpsertIssue is a backward-compatible wrapper around UpsertIssueWithOutcome
// that discards the outcome. Kept because tests/integration/processor_store_test.go
// calls it directly with the single-error signature.
func (s *pgStore) UpsertIssue(ctx context.Context, issue *Issue, releaseVersion string) error {
	_, err := s.UpsertIssueWithOutcome(ctx, issue, releaseVersion)
	return err
}

func (s *pgStore) UpsertIssueWithOutcome(ctx context.Context, issue *Issue, releaseVersion string) (IssueOutcome, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return IssueOutcomeExisting, err
	}
	defer tx.Rollback(ctx)

	// Read current issue state for regression evaluation if issue exists
	var existingStatus, existingResolvedInVersion string
	var existingID string

	err = tx.QueryRow(ctx,
		"SELECT id, status, COALESCE(resolved_in_version, '') FROM issues WHERE project_id = $1 AND fingerprint = $2",
		issue.ProjectID, issue.Fingerprint,
	).Scan(&existingID, &existingStatus, &existingResolvedInVersion)

	// Only "no such issue yet" is expected here. Any other error means the statement genuinely
	// failed, and once a statement fails inside a transaction Postgres aborts it — every subsequent
	// statement returns 25P02 "current transaction is aborted". Falling through on a real error
	// therefore REPLACED the actual cause (e.g. `relation "issues" does not exist`) with a generic
	// abort message, which is worse than useless when diagnosing a live pipeline.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return IssueOutcomeExisting, fmt.Errorf("failed to read existing issue for project=%s fingerprint=%s: %w",
			issue.ProjectID, issue.Fingerprint, err)
	}

	wasNewIssue := errors.Is(err, pgx.ErrNoRows)
	isRegressed := false
	if err == nil {
		issue.ID = existingID
		if existingStatus == "resolved" {
			if existingResolvedInVersion == "" || isRegressionVersion(releaseVersion, existingResolvedInVersion) {
				isRegressed = true
			}
		}
	}

	if isRegressed {
		query := `
			UPDATE issues SET
				status = 'unresolved',
				regression_status = 'regressed',
				regression_count = regression_count + 1,
				last_regressed_at = NOW(),
				resolved_in_version = NULL,
				resolved_at = NULL,
				resolved_by_type = NULL,
				resolved_by = NULL,
				last_seen = GREATEST(issues.last_seen, $1),
				count = issues.count + 1
			WHERE id = $2
		`
		if _, err := tx.Exec(ctx, query, issue.LastSeen, issue.ID); err != nil {
			return IssueOutcomeExisting, err
		}

		activityMeta, _ := json.Marshal(map[string]string{
			"releaseVersion":          releaseVersion,
			"previousResolvedVersion": existingResolvedInVersion,
		})

		// issue_activity has no `metadata` column — the schema
		// (1721900000_add_issue_lifecycle_and_relations.sql:83-92) defines old_value/new_value. Inserting
		// `metadata` raised 42703 inside this transaction, so EVERY regression event lost both the issue
		// update and its occurrence, and errors.go classifies 42703 as retryable so it burned the full
		// delivery budget before dead-lettering. U11 failed 100%.
		activityQuery := `
			INSERT INTO issue_activity (id, issue_id, actor_type, actor_id, event_type, new_value, created_at)
			VALUES (gen_random_uuid(), $1, 'system', 'sentinel-regression-detector', 'regressed', $2, NOW())
		`
		if _, err := tx.Exec(ctx, activityQuery, issue.ID, activityMeta); err != nil {
			return IssueOutcomeExisting, err
		}
	} else {
		query := `
			INSERT INTO issues (id, project_id, fingerprint, message, error_class, status, first_seen, last_seen, count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)
			ON CONFLICT (project_id, fingerprint)
			DO UPDATE SET
				last_seen = GREATEST(issues.last_seen, EXCLUDED.last_seen),
				count = issues.count + 1
			WHERE ` + issuesUpsertConflictPredicate + `
		`
		if _, err := tx.Exec(ctx, query,
			issue.ID,
			issue.ProjectID,
			issue.Fingerprint,
			issue.Message,
			issue.ErrorClass,
			issue.Status,
			issue.FirstSeen,
			issue.LastSeen,
		); err != nil {
			return IssueOutcomeExisting, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return IssueOutcomeExisting, err
	}

	switch {
	case isRegressed:
		return IssueOutcomeRegressed, nil
	case wasNewIssue:
		return IssueOutcomeNew, nil
	default:
		return IssueOutcomeExisting, nil
	}
}

// StoreEvent is the atomic write path for one delivered event (docs/plans/IDEMPOTENCY_PLAN.md D-c,
// followed verbatim — every clause below is load-bearing and was probed against a real Postgres
// during the plan's adversarial review; do not "simplify" any of them without re-reading D-c first).
//
// One transaction, explicitly READ COMMITTED (pgx's default happens to already be READ COMMITTED, but
// the isolation level is spelled out anyway: D-c's interleaving THROWS 40001 under REPEATABLE READ/
// SERIALIZABLE, which classifyStoreError leaves retryable — a hot fingerprint would turn contention
// into a NAK/retry storm. Never remove the explicit TxOptions on the theory that the default already
// matches it):
//  1. Read existing issue state; upsert/update exactly as UpsertIssueWithOutcome did, but with
//     RETURNING id folded into BOTH arms so the separate GetIssueIDByFingerprint round trip disappears.
//     BOTH arms check rows affected: a folded RETURNING id yielding pgx.ErrNoRows is a BUG (the
//     DO UPDATE...WHERE predicate was narrowed, or the plain UPDATE hit a concurrently-deleted issue),
//     never a duplicate signal — it is surfaced as an error, not stored=false.
//  2. Insert the occurrence with NULLIF($11,”) mapping ” (proto3's "absent", never a real key) to SQL
//     NULL, and ON CONFLICT (issue_id, event_id) WHERE event_id IS NOT NULL DO NOTHING — the exact form
//     that matches the D-b partial unique index. Dropping NULLIF silently destroys every pre-W0 event
//     after the first per issue (F-TX-1); dropping the WHERE clause, or using a bare
//     ON CONFLICT DO NOTHING, either fails to match the index or swallows an
//     error_occurrences_pkey collision as a false "duplicate" (F-TX-5) — both are forbidden here.
//     Executed via tx.Exec, never QueryRow+RETURNING: pgx.ErrNoRows is not a *pgconn.PgError, so
//     classifyStoreError would leave a genuine duplicate retryable and eventually dead-letter an
//     already-stored message (F-TX-8).
//  3. RowsAffected()==0 on the occurrence insert means this exact (issue_id, event_id) pair already
//     exists: roll back (undoing this delivery's count/regression writes) and report stored=false with
//     a nil error — the caller ACKs this as a successful no-op. Otherwise commit.
//
// Deadlock-freedom depends on this transaction acquiring exactly ONE contended lock (the issues row,
// via ON CONFLICT DO UPDATE / the plain UPDATE) and never waiting again — do not add audit persistence,
// indexing, or any network I/O inside this transaction; move it after commit instead (D-d).
//
// The IssueOutcome return is BEST-EFFORT and currently consumed by nobody — every call site discards
// it (verified at ship time). Its New/Existing distinction comes from a plain SELECT before the
// upsert, so under a first-insert race two concurrent deliveries can both report IssueOutcomeNew;
// the duplicate path always reports IssueOutcomeExisting regardless of what the original delivery
// was. If you are about to build behavior on this value, either accept those semantics explicitly or
// derive the distinction inside the upsert (e.g. xmax = 0 on the RETURNING row) first — do not assume
// it is exact.
func (s *pgStore) StoreEvent(ctx context.Context, issue *Issue, occ *ErrorOccurrence, releaseVersion string) (IssueOutcome, bool, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return IssueOutcomeExisting, false, err
	}
	defer tx.Rollback(ctx)

	// --- 1. Issue upsert / regression bookkeeping (UpsertIssueWithOutcome's logic, folded) ---

	var existingStatus, existingResolvedInVersion string
	var existingID string

	err = tx.QueryRow(ctx,
		"SELECT id, status, COALESCE(resolved_in_version, '') FROM issues WHERE project_id = $1 AND fingerprint = $2",
		issue.ProjectID, issue.Fingerprint,
	).Scan(&existingID, &existingStatus, &existingResolvedInVersion)

	// Only "no such issue yet" is expected here; see UpsertIssueWithOutcome's identical comment for why
	// falling through on any other error would replace the real cause with a generic 25P02 abort.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return IssueOutcomeExisting, false, fmt.Errorf("failed to read existing issue for project=%s fingerprint=%s: %w",
			issue.ProjectID, issue.Fingerprint, err)
	}

	wasNewIssue := errors.Is(err, pgx.ErrNoRows)
	isRegressed := false
	if err == nil {
		issue.ID = existingID
		if existingStatus == "resolved" {
			if existingResolvedInVersion == "" || isRegressionVersion(releaseVersion, existingResolvedInVersion) {
				isRegressed = true
			}
		}
	}

	if isRegressed {
		query := `
			UPDATE issues SET
				status = 'unresolved',
				regression_status = 'regressed',
				regression_count = regression_count + 1,
				last_regressed_at = NOW(),
				resolved_in_version = NULL,
				resolved_at = NULL,
				resolved_by_type = NULL,
				resolved_by = NULL,
				last_seen = GREATEST(issues.last_seen, $1),
				count = issues.count + 1
			WHERE id = $2
			RETURNING id
		`
		var updatedID string
		if scanErr := tx.QueryRow(ctx, query, issue.LastSeen, issue.ID).Scan(&updatedID); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				// F-TX-7: the row this UPDATE targeted no longer exists — e.g. a concurrent
				// retention DELETE won the race against this tx's own SELECT above. This is a bug
				// condition, never a duplicate: report it loudly instead of silently proceeding
				// with a stale issue.ID that would fail the occurrence insert's FK with a confusing
				// error, or worse, succeed against a row that means something else now.
				return IssueOutcomeExisting, false, fmt.Errorf(
					"regression update matched no row for issue id=%s (concurrently deleted?): %w",
					issue.ID, scanErr)
			}
			return IssueOutcomeExisting, false, scanErr
		}
		issue.ID = updatedID

		activityMeta, _ := json.Marshal(map[string]string{
			"releaseVersion":          releaseVersion,
			"previousResolvedVersion": existingResolvedInVersion,
		})

		// issue_activity has no `metadata` column — see UpsertIssueWithOutcome's identical comment
		// (VERIFIED_STATE.md S-something / the 1721900000 schema) for why old_value/new_value is used.
		activityQuery := `
			INSERT INTO issue_activity (id, issue_id, actor_type, actor_id, event_type, new_value, created_at)
			VALUES (gen_random_uuid(), $1, 'system', 'sentinel-regression-detector', 'regressed', $2, NOW())
		`
		if _, err := tx.Exec(ctx, activityQuery, issue.ID, activityMeta); err != nil {
			return IssueOutcomeExisting, false, err
		}
	} else {
		// N7a (A01/A06): RETURNING (xmax = 0) AS inserted gives an EXACT new-vs-existing signal
		// from the upsert statement itself, unlike wasNewIssue above (a pre-upsert SELECT in the
		// same tx — see the outcome-inexactness caveat in this method's doc comment, and
		// store.go:309-315 for UpsertIssueWithOutcome's equivalent). xmax=0 means this row's
		// current version was created by THIS command (a fresh INSERT), never by the DO UPDATE
		// arm — so it cannot alias a concurrent-race duplicate the way wasNewIssue can.
		query := `
			INSERT INTO issues (id, project_id, fingerprint, message, error_class, status, first_seen, last_seen, count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)
			ON CONFLICT (project_id, fingerprint)
			DO UPDATE SET
				last_seen = GREATEST(issues.last_seen, EXCLUDED.last_seen),
				count = issues.count + 1
			WHERE ` + issuesUpsertConflictPredicate + `
			RETURNING id, (xmax = 0) AS inserted
		`
		var returnedID string
		var wasFreshInsert bool
		if scanErr := tx.QueryRow(ctx, query,
			issue.ID,
			issue.ProjectID,
			issue.Fingerprint,
			issue.Message,
			issue.ErrorClass,
			issue.Status,
			issue.FirstSeen,
			issue.LastSeen,
		).Scan(&returnedID, &wasFreshInsert); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				// F-TX-7: the folded RETURNING id yielded no row. The only way this happens is the
				// DO UPDATE ... WHERE predicate being narrowed so it no longer matches — a bug in
				// this statement, not "the issue was a duplicate". Must error, never stored=false.
				return IssueOutcomeExisting, false, fmt.Errorf(
					"issue upsert returned no row for project=%s fingerprint=%s (WHERE predicate narrowed?): %w",
					issue.ProjectID, issue.Fingerprint, scanErr)
			}
			return IssueOutcomeExisting, false, scanErr
		}
		issue.ID = returnedID

		if wasFreshInsert {
			// A01/A06: agents currently have no reliable "a new issue exists" signal in the
			// events feed. One 'created' row per genuinely-new issue, inside this same tx so a
			// duplicate-occurrence rollback below undoes it too.
			activityMeta, _ := json.Marshal(map[string]string{
				"errorClass": issue.ErrorClass,
				"projectId":  issue.ProjectID,
			})
			createdQuery := `
				INSERT INTO issue_activity (id, issue_id, actor_type, actor_id, event_type, new_value, created_at)
				VALUES (gen_random_uuid(), $1, 'system', 'sentinel-processor', 'created', $2, NOW())
			`
			if _, err := tx.Exec(ctx, createdQuery, issue.ID, activityMeta); err != nil {
				return IssueOutcomeExisting, false, err
			}
		} else {
			// R2: repeat-occurrence discovery signal, throttled to at most one 'occurrence_burst'
			// row per issue per occurrenceEventMinInterval — a single INSERT ... SELECT guarded by
			// NOT EXISTS, same tx, no new contended lock (the NOT EXISTS only reads). Skipped
			// entirely when a 'created' or 'regressed' row already covers this issue recently
			// (isRegressed always takes the other branch above, so a same-delivery regression can
			// never race this — a 'regressed' row here can only be from an earlier delivery).
			burstQuery := `
				INSERT INTO issue_activity (id, issue_id, actor_type, actor_id, event_type, new_value, created_at)
				SELECT gen_random_uuid(), $1, 'system', 'sentinel-processor', 'occurrence_burst',
					jsonb_build_object('count', issues.count, 'lastSeen', issues.last_seen), NOW()
				FROM issues
				WHERE issues.id = $1
				AND NOT EXISTS (
					SELECT 1 FROM issue_activity
					WHERE issue_activity.issue_id = $1
					AND issue_activity.event_type IN ('created', 'occurrence_burst', 'regressed')
					AND issue_activity.created_at > NOW() - make_interval(secs => $2)
				)
			`
			if _, err := tx.Exec(ctx, burstQuery, issue.ID, occurrenceEventMinInterval.Seconds()); err != nil {
				return IssueOutcomeExisting, false, err
			}
		}
	}

	// --- 2. Occurrence insert, deduplicated on (issue_id, event_id) ---

	occ.IssueID = issue.ID
	occQuery := `
		INSERT INTO error_occurrences (id, issue_id, environment, platform, release_version,
			stacktrace, metadata, trace_id, span_id, created_at, event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, NULLIF($11,''))
		ON CONFLICT (issue_id, event_id) WHERE event_id IS NOT NULL DO NOTHING
	`
	tag, err := tx.Exec(ctx, occQuery,
		occ.ID,
		occ.IssueID,
		occ.Environment,
		occ.Platform,
		occ.ReleaseVersion,
		occ.Stacktrace,
		occ.Metadata,
		occ.TraceID,
		occ.SpanID,
		occ.CreatedAt,
		occ.EventID,
	)
	if err != nil {
		return IssueOutcomeExisting, false, err
	}

	// --- 3. Duplicate detection strictly from RowsAffected (F-TX-8) ---

	if tag.RowsAffected() == 0 {
		// This exact (issue_id, event_id) pair is already stored. Roll back everything this
		// delivery would otherwise have written — the issue's count/regression bookkeeping above
		// included — so a redelivered duplicate leaves every counter untouched (D-c property 3).
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return IssueOutcomeExisting, false, fmt.Errorf("failed to roll back duplicate occurrence insert: %w", rbErr)
		}
		return IssueOutcomeExisting, false, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return IssueOutcomeExisting, false, err
	}

	switch {
	case isRegressed:
		return IssueOutcomeRegressed, true, nil
	case wasNewIssue:
		return IssueOutcomeNew, true, nil
	default:
		return IssueOutcomeExisting, true, nil
	}
}

func (s *pgStore) InsertOccurrence(ctx context.Context, occ *ErrorOccurrence) error {
	query := `
		INSERT INTO error_occurrences (id, issue_id, environment, platform, release_version, stacktrace, metadata, trace_id, span_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`

	_, err := s.db.Exec(ctx, query,
		occ.ID,
		occ.IssueID,
		occ.Environment,
		occ.Platform,
		occ.ReleaseVersion,
		occ.Stacktrace,
		occ.Metadata,
		occ.TraceID,
		occ.SpanID,
		occ.CreatedAt,
	)

	return err
}

// ResolveProjectID resolves the tenant project for an incoming event.
//
// It prefers projectID — ErrorEvent.project_id, proto field 16 — when it is
// non-empty, resolving it directly against projects.id. Only when it is
// empty does it fall back to the legacy GetProjectByKey name lookup.
//
// This matters because for the Go SDK (and any client following
// docs/sdk-specification.md as originally written), ProjectKey/project_key
// carries the API key string used for authentication, not a projects.name
// value — GetProjectByKey's "SELECT id FROM projects WHERE name = $1" was
// therefore guaranteed to fail for real SDK traffic with
// "project not found: <api-key>" (VERIFIED_STATE.md S6). Once a caller
// populates project_id (the ingestor's job, from the authenticated API key
// context — see docs/plans/E2E_RECOVERY_PLAN.md P3-1, which is NOT done by
// this change), this resolves correctly without going through the name
// lookup at all. Until then, this is a no-op for callers that still leave
// project_id empty: they keep getting exactly today's fallback behavior.
func (s *pgStore) ResolveProjectID(ctx context.Context, projectID, projectKey string) (string, error) {
	if projectID == "" {
		return s.GetProjectByKey(ctx, projectKey)
	}

	var id string
	err := s.db.QueryRow(ctx,
		"SELECT id FROM projects WHERE id = $1",
		projectID,
	).Scan(&id)

	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("project not found: %s", projectID)
	}
	return id, err
}

func (s *pgStore) GetProjectByKey(ctx context.Context, projectKey string) (string, error) {
	var projectID string
	err := s.db.QueryRow(ctx,
		"SELECT id FROM projects WHERE name = $1",
		projectKey,
	).Scan(&projectID)

	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("project not found: %s", projectKey)
	}
	return projectID, err
}

func (s *pgStore) GetIssueIDByFingerprint(ctx context.Context, projectID, fingerprint string) (string, error) {
	var issueID string
	err := s.db.QueryRow(ctx,
		"SELECT id FROM issues WHERE project_id = $1 AND fingerprint = $2",
		projectID, fingerprint,
	).Scan(&issueID)
	return issueID, err
}
