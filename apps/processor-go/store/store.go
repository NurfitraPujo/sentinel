package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/procmetrics"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
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
	// GetIssueIDByFingerprint takes `environment` because sentinel-project-environments §4.9 makes
	// (project_id, environment, fingerprint) the issue identity. StoreEvent does NOT call this — it
	// has its own inline SELECT — so changing only this function would leave the write path grouping
	// across environments while every reader believed it did not.
	GetIssueIDByFingerprint(ctx context.Context, projectID, environment, fingerprint string) (string, error)
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

// issuesUpsertConflictPredicate scopes the issues ON CONFLICT (project_id, environment,
// fingerprint) DO UPDATE to processor-owned rows only. The processor never writes issue_type, but a
// manual (issue_type = 'user_report') issue could theoretically collide on the conflict target;
// without this predicate the processor's upsert would bump last_seen/count on that manual issue
// and Dispatcher.Dispatch would fire for it. See R13, docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md §10.
//
// sentinel-project-environments §4.9 widens the conflict TARGET and deliberately leaves this text
// alone. `issue_type = 'system_error'` names an OWNERSHIP distinction, not an identity one, and
// §4.8a keeps both dashboard writers writing 'user_report' rows — so the set this excludes is
// exactly the set it excluded before. `issues.fingerprint = EXCLUDED.fingerprint` was already
// implied by the old conflict target and stays implied by the wider one. The collision it guards
// against becomes strictly RARER (a manual issue's fingerprint is 16 random bytes and now has to
// match on environment too), and rarer is not never.
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
	ID        string
	ProjectID string
	// Environment is part of the issue's identity as of sentinel-project-environments §4.9:
	// `issues` is UNIQUE (project_id, environment, fingerprint) and the column is NOT NULL with no
	// DEFAULT, so every write path must supply it. It comes from the event (§4.8), never from the
	// registry — an environment may legitimately exist in the data and not in project_environments.
	Environment       string
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
	// AffectedUserID / AffectedTenantID feed sentinel-affected-users §4.3's rollup: one
	// issue_affected_users / issue_affected_tenants row per distinct (issue, id) pair, and a bump of
	// issues.affected_user_count / affected_tenant_count only when the pair is NEW. Carried exactly as
	// EventID is: read by event.Normalize from the PRE-normalisation metadata (decision 3 — after
	// normalisation a UUID reads "<UUID>"), bounds-clamped by event.BoundAffectedID to "" for anything
	// empty, over 255 bytes or carrying a control character (decision 5), and NEVER normalised or
	// masked (decision 6: identity is the exact byte string). "" here means "nothing usable" — StoreEvent
	// runs no side-table statement for it; the CHECK (<> '') on both tables is the tripwire that makes
	// a regression of that loud. Never a metric label (D15): client-supplied, unbounded.
	AffectedUserID   string
	AffectedTenantID string
	// AffectedUserSkipReason / AffectedTenantSkipReason are "" when the id above is usable or no key
	// was present at all, and one of obs.AffectedRollupReason* (empty | too_long | control_char) when a
	// key WAS present and decision 5 dropped its value. StoreEvent records each non-empty reason on
	// sentinel_affected_rollup_skipped_total{kind,reason} AFTER commit (§12), so a delivery that rolled
	// back — a duplicate above all — never counts.
	AffectedUserSkipReason   string
	AffectedTenantSkipReason string
}

// The §4.3 rollup statements, one pair per side table. RETURNING (xmax = 0) AS inserted is the same
// exact new-vs-existing signal the 'created' activity row above already relies on: xmax = 0 means
// this row version was created by THIS command — a fresh INSERT — never by the DO UPDATE arm, so it
// cannot alias a race the way a pre-SELECT can. GREATEST on last_seen mirrors issues.last_seen;
// LEAST on first_seen exists because event timestamps are client-supplied and may arrive out of
// order — first_seen is "earliest occurrence from this user we stored", not "first delivery".
// $3 is the occurrence's created_at (decision 9), never now(): retention compares occurrences by
// created_at, and a last_seen on a different clock would let a user outlive every occurrence that
// justified the row.
const (
	affectedUsersUpsertQuery = `
		INSERT INTO issue_affected_users (issue_id, user_id, first_seen, last_seen)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (issue_id, user_id) DO UPDATE
		   SET last_seen  = GREATEST(issue_affected_users.last_seen, EXCLUDED.last_seen),
		       first_seen = LEAST(issue_affected_users.first_seen, EXCLUDED.first_seen)
		RETURNING (xmax = 0) AS inserted
	`
	affectedUsersBumpQuery = `UPDATE issues SET affected_user_count = affected_user_count + 1 WHERE id = $1`

	affectedTenantsUpsertQuery = `
		INSERT INTO issue_affected_tenants (issue_id, tenant_id, first_seen, last_seen)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (issue_id, tenant_id) DO UPDATE
		   SET last_seen  = GREATEST(issue_affected_tenants.last_seen, EXCLUDED.last_seen),
		       first_seen = LEAST(issue_affected_tenants.first_seen, EXCLUDED.first_seen)
		RETURNING (xmax = 0) AS inserted
	`
	affectedTenantsBumpQuery = `UPDATE issues SET affected_tenant_count = affected_tenant_count + 1 WHERE id = $1`
)

// rollupAffected runs one side table's pair of §4.3 statements inside tx: the upsert, then — only
// when the upsert INSERTed — the count bump. Not savepointed (decision 8): unlike the registry touch,
// the counts are a first-class fact, and a genuine error here (connection, constraint, disk) fails
// the transaction so NATS redelivers the event, exactly as an occurrence-insert error does today. The
// only deterministic error the value could cause — length or a control character — is prevented by
// event.BoundAffectedID before the caller ever reaches this.
func rollupAffected(ctx context.Context, tx pgx.Tx, upsertQuery, bumpQuery, issueID, id string, seenAt time.Time) error {
	var inserted bool
	if err := tx.QueryRow(ctx, upsertQuery, issueID, id, seenAt).Scan(&inserted); err != nil {
		return fmt.Errorf("affected rollup upsert failed for issue id=%s: %w", issueID, err)
	}
	if !inserted {
		return nil
	}
	if _, err := tx.Exec(ctx, bumpQuery, issueID); err != nil {
		return fmt.Errorf("affected rollup count bump failed for issue id=%s: %w", issueID, err)
	}
	return nil
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
		"SELECT id, status, COALESCE(resolved_in_version, '') FROM issues WHERE project_id = $1 AND environment = $2 AND fingerprint = $3",
		issue.ProjectID, issue.Environment, issue.Fingerprint,
	).Scan(&existingID, &existingStatus, &existingResolvedInVersion)

	// Only "no such issue yet" is expected here. Any other error means the statement genuinely
	// failed, and once a statement fails inside a transaction Postgres aborts it — every subsequent
	// statement returns 25P02 "current transaction is aborted". Falling through on a real error
	// therefore REPLACED the actual cause (e.g. `relation "issues" does not exist`) with a generic
	// abort message, which is worse than useless when diagnosing a live pipeline.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return IssueOutcomeExisting, fmt.Errorf("failed to read existing issue for project=%s environment=%s fingerprint=%s: %w",
			issue.ProjectID, issue.Environment, issue.Fingerprint, err)
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
			INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, status, first_seen, last_seen, count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1)
			ON CONFLICT (project_id, environment, fingerprint)
			DO UPDATE SET
				last_seen = GREATEST(issues.last_seen, EXCLUDED.last_seen),
				count = issues.count + 1
			WHERE ` + issuesUpsertConflictPredicate + `
		`
		if _, err := tx.Exec(ctx, query,
			issue.ID,
			issue.ProjectID,
			issue.Environment,
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
//     a nil error — the caller ACKs this as a successful no-op. Otherwise continue.
//  4. The affected users/tenants rollup (sentinel-affected-users §4.3): per present identifier, an
//     upsert into issue_affected_users / issue_affected_tenants keyed (issue_id, id) and — only when
//     that upsert INSERTed (RETURNING xmax = 0) — a +1 on issues.affected_user_count /
//     affected_tenant_count. After the duplicate check so a deduplicated redelivery never reaches it;
//     before the registry touch; NOT savepointed (decision 8).
//  5. The project_environments registry touch (sentinel-project-environments §4.8), savepointed.
//     Then commit.
//
// Deadlock-freedom depends on this transaction acquiring its ONE contended lock (the issues row, via
// ON CONFLICT DO UPDATE / the plain UPDATE) FIRST and holding it to commit — do not add audit
// persistence, indexing, or any network I/O inside this transaction; move it after commit instead
// (D-d). Two writes are the DELIBERATE exceptions to that sentence, and both were added knowing it:
//
//   - the registry touch of step 5 (sentinel-project-environments decision 9) — the first exception,
//     best-effort behind a savepoint; see its own block comment below.
//   - the affected users/tenants rollup of step 4 (sentinel-affected-users decision 2) — the SECOND
//     deliberate exception, and unlike the first it is a first-class fact, not best-effort. It is
//     inside this transaction because a count that can move without an occurrence behind it is the
//     S18 defect again, on a new column: written after commit, a NAK between the occurrence commit
//     and the rollup write would either lose a user (never bumped) or, on retry, find the occurrence
//     deduplicated and skip the rollup forever. Inside, both land or neither does. The property the
//     "one lock" rule protects survives: every StoreEvent transaction takes the issues row lock first
//     and holds it, so two deliveries for the same issue are already serialised before either touches
//     a side-table row, two deliveries for different issues share no row, and the retention sweep
//     takes the same order (issues FOR UPDATE, then the side table — §4.5, decision 23). No cycle is
//     possible between any two of these transactions. Cost, stated: an occurrence from a NEW user
//     writes the issues row twice in one transaction (the upsert, then the bump).
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
		"SELECT id, status, COALESCE(resolved_in_version, '') FROM issues WHERE project_id = $1 AND environment = $2 AND fingerprint = $3",
		issue.ProjectID, issue.Environment, issue.Fingerprint,
	).Scan(&existingID, &existingStatus, &existingResolvedInVersion)

	// Only "no such issue yet" is expected here; see UpsertIssueWithOutcome's identical comment for why
	// falling through on any other error would replace the real cause with a generic 25P02 abort.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return IssueOutcomeExisting, false, fmt.Errorf("failed to read existing issue for project=%s environment=%s fingerprint=%s: %w",
			issue.ProjectID, issue.Environment, issue.Fingerprint, err)
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
			INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, status, first_seen, last_seen, count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1)
			ON CONFLICT (project_id, environment, fingerprint)
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
			issue.Environment,
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
					"issue upsert returned no row for project=%s environment=%s fingerprint=%s (WHERE predicate narrowed?): %w",
					issue.ProjectID, issue.Environment, issue.Fingerprint, scanErr)
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

	// --- 4. Affected users / tenants rollup (sentinel-affected-users §4.3, decisions 2, 8, 9) ---
	//
	// The second deliberate in-transaction exception named in this method's doc comment. Position is
	// load-bearing: AFTER the duplicate rollback above so a redelivered (issue_id, event_id) never
	// writes a side row or bumps a count (flow I5), BEFORE the registry touch, and NOT inside a
	// savepoint — a failure here is a failure to store the event, returned so NATS retries (decision
	// 8). An event with neither identifier runs no statement at all; an identifier decision 5 dropped
	// is "" here and is counted after commit, below.
	if occ.AffectedUserID != "" {
		if err := rollupAffected(ctx, tx, affectedUsersUpsertQuery, affectedUsersBumpQuery, issue.ID, occ.AffectedUserID, occ.CreatedAt); err != nil {
			return IssueOutcomeExisting, false, err
		}
	}
	if occ.AffectedTenantID != "" {
		if err := rollupAffected(ctx, tx, affectedTenantsUpsertQuery, affectedTenantsBumpQuery, issue.ID, occ.AffectedTenantID, occ.CreatedAt); err != nil {
			return IssueOutcomeExisting, false, err
		}
	}

	// --- 5. Registry touch (sentinel-project-environments §4.8, decisions 8 and 9) ---
	//
	// An UPDATE, never an INSERT. Under decision 8 a project_environments row exists because a
	// person declared it, so ingest creates none. Four properties, each of which fails silently if
	// got wrong:
	//
	//  1. Zero rows updated is NOT an error — it means this environment is not registered, and
	//     decision 9 says ACCEPT the event anyway. The occurrence is already written, the
	//     transaction commits, and sentinel_unregistered_environment_events_total is incremented.
	//     An error tracker that drops errors because a settings table is out of date is worse than
	//     the problem it is solving.
	//  1a. Neither is a FAILING UPDATE an error the caller ever sees. §4.8 point 1 says "ingest
	//     never fails because of the registry" without qualification, and §12 states the
	//     consequence of this statement erroring as first_seen_at/last_seen_at drifting from
	//     reality — not as a lost event. So the failure is logged, counted on
	//     sentinel_environment_registry_touch_failures_total, and the transaction still commits.
	//     Returning the error here dropped the event, which is the exact inversion point 1 names.
	//     Postgres aborts a transaction on any statement error, so the touch runs inside a
	//     SAVEPOINT (pgx models a nested Begin as one): rolling back TO that savepoint restores
	//     the enclosing transaction — with the issue and the occurrence intact — so it can commit.
	//     This keeps the UPDATE inside the transaction (property 2) rather than dodging its lock.
	//  2. It is inside this transaction, so a last_seen_at that moved with no occurrence behind it
	//     is impossible.
	//  3. It runs AFTER the duplicate check above, so a deduplicated redelivery never reaches it.
	//     The rollback would undo the UPDATE anyway — but decision 9 pairs this statement with a
	//     counter, and no rollback can un-increment a counter, so "never reached" is the property
	//     to hold rather than "undone".
	//  4. There is NO cap enforcement here. MAX_ENVIRONMENTS_PER_PROJECT is enforced at §4.3 and
	//     nowhere else, precisely because ingest no longer creates rows.
	//
	// One thing a future reader will look for and not find:
	//
	//   - this does add a second row lock to a transaction whose doc comment above says it acquires
	//     exactly one. That is deliberate and contract §12 names it: "last_seen_at turns
	//     project_environments into a hot row". It is one indexed single-row UPDATE on a tiny table,
	//     taken after every other write in this transaction, so it cannot introduce a lock-ordering
	//     cycle with the issues row; §13's M3 reports on its cost and deliberately does not gate on
	//     it. Do not move it out of the transaction to avoid the lock — property 2 is why it is here.
	registryQuery := `
		UPDATE project_environments
		   SET last_seen_at  = now(),
		       first_seen_at = COALESCE(first_seen_at, now())
		 WHERE project_id = $1 AND name = $2
	`
	unregisteredEnvironment := false
	if touchErr := func() error {
		// The savepoint of point 1a. Its Commit is a RELEASE SAVEPOINT, its Rollback a
		// ROLLBACK TO SAVEPOINT — neither ends the enclosing transaction.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		tag, err := sp.Exec(ctx, registryQuery, issue.ProjectID, occ.Environment)
		if err != nil {
			_ = sp.Rollback(ctx)
			return err
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		unregisteredEnvironment = tag.RowsAffected() == 0
		return nil
	}(); touchErr != nil {
		slog.ErrorContext(ctx, "environment registry touch failed; the event is still stored (contract §4.8 point 1)",
			slog.String("project_id", issue.ProjectID),
			slog.String("environment", occ.Environment),
			slog.String("error", touchErr.Error()))
		procmetrics.RecordEnvironmentRegistryTouchFailure(ctx)
	}

	if err := tx.Commit(ctx); err != nil {
		return IssueOutcomeExisting, false, err
	}

	// Recorded after the commit: the metric counts occurrences that were STORED for an environment
	// nobody declared, so an event that failed to commit must not appear on it.
	if unregisteredEnvironment {
		procmetrics.RecordUnregisteredEnvironmentEvent(ctx)
	}
	// Same rule for the rollup's skip counter (sentinel-affected-users §12): it describes identifiers
	// skipped on occurrences that were STORED. A duplicate returned at step 3 and a failed commit both
	// leave it untouched.
	if occ.AffectedUserSkipReason != "" {
		procmetrics.RecordAffectedRollupSkipped(ctx, obs.AffectedKindUser, occ.AffectedUserSkipReason)
	}
	if occ.AffectedTenantSkipReason != "" {
		procmetrics.RecordAffectedRollupSkipped(ctx, obs.AffectedKindTenant, occ.AffectedTenantSkipReason)
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

func (s *pgStore) GetIssueIDByFingerprint(ctx context.Context, projectID, environment, fingerprint string) (string, error) {
	var issueID string
	err := s.db.QueryRow(ctx,
		"SELECT id FROM issues WHERE project_id = $1 AND environment = $2 AND fingerprint = $3",
		projectID, environment, fingerprint,
	).Scan(&issueID)
	return issueID, err
}
