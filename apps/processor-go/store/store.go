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
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
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
	// LookupSourcemap is sentinel-sourcemaps §4.4 step 3: the symbolication stage's read of
	// `sourcemaps` on §4.5's UNIQUE (project_id, release_version, minified_path). It has
	// symbolicate.Store's Lookup signature so the service wires it as
	// symbolicate.StoreFunc(s.store.LookupSourcemap). (nil, nil) is "no row" -- the stage's
	// `no_map`. See the method doc on pgStore.LookupSourcemap for the tenancy rule.
	LookupSourcemap(ctx context.Context, projectID, releaseVersion, minifiedPath string) (*symbolicate.Sourcemap, error)
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

// IssueOutcome is the class of the issue row's status AFTER the write (sentinel-releases-snooze
// §4.3, decision 29). StoreEvent returns all five values; UpsertIssueWithOutcome (the pre-idempotency
// path, no snooze arm) keeps its three-valued behaviour and never returns the two newest.
type IssueOutcome int

const (
	// IssueOutcomeExisting means the DO UPDATE arm ran and the row's status after the write is
	// 'unresolved' or 'resolved' — a non-regressing recurrence of an issue that already existed.
	IssueOutcomeExisting IssueOutcome = iota
	// IssueOutcomeNew means no issue existed for this fingerprint before
	// this call.
	IssueOutcomeNew
	// IssueOutcomeRegressed means a previously resolved issue reappeared.
	IssueOutcomeRegressed
	// IssueOutcomeUnsnoozed means the snooze arm ran: the issue was 'ignored' with a condition
	// (a date or an occurrence target) that this occurrence met, and the row is now 'unresolved'
	// with the three snooze columns NULL (decision 17). The service dispatches it normally, through
	// the threshold — neither bypassed nor withheld (decision 29).
	IssueOutcomeUnsnoozed
	// IssueOutcomeIgnored means the DO UPDATE arm ran and the row's status after the write is
	// 'ignored' — a snooze still running, or a plain indefinite ignore. Read from RETURNING status on
	// the upsert, never from the step-1 snapshot. It replaces IssueOutcomeExisting exactly for that
	// case; the service skips dispatchAlert iff this value (decision 16).
	IssueOutcomeIgnored
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
	// ReleaseInserted is WRITTEN BACK by StoreEvent, never read by it — the same idiom as IssueID
	// above. It is true exactly when this delivery committed and step 4c's `releases` upsert INSERTed
	// the (project_id, ReleaseVersion) row (RETURNING xmax = 0): a first sighting of that version by
	// the project, with first_seen = LEAST(CreatedAt, now()) (decision 50). It is the signal
	// sentinel-releases-snooze §4.3's post-commit `next` rewrite runs on (decision 15) — surfaced
	// here rather than as a fourth return value so the CommandStore signature and its ~40 call sites
	// stay as they are. False on every other path: an empty ReleaseVersion, a duplicate
	// (stored = false), an error, or the DO UPDATE arm (the version was seen before). Under
	// concurrent first sightings ON CONFLICT arbitrates and exactly one delivery reads true
	// (decision 4).
	ReleaseInserted bool
	// ReleaseFirstSeen is WRITTEN BACK beside ReleaseInserted and meaningful only when it is true:
	// the `releases` row's first_seen as step 4c actually wrote it (RETURNING first_seen), which is
	// CreatedAt clamped to the transaction's now() (decision 50). The post-commit `next` rewrite
	// MUST compare resolved_at against this, never against CreatedAt: a future client clock would
	// otherwise fan a version out to every waiting `next` issue of the project on the strength of a
	// first_seen the row does not hold. Zero on every path where ReleaseInserted is false.
	ReleaseFirstSeen time.Time
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

// occurrenceBucketUpsertQuery is sentinel-issue-trend §4.3's one statement, verbatim: $1 = issue.ID,
// $2 = occ.CreatedAt.UTC().Truncate(time.Hour) (the UTC hour, computed in Go — §3, decision 2). The
// conflict target is the table's composite PRIMARY KEY (issue_id, bucket_start).
const occurrenceBucketUpsertQuery = `
		INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count)
		VALUES ($1, $2, 1)
		ON CONFLICT (issue_id, bucket_start) DO UPDATE
		   SET count = issue_occurrence_buckets.count + 1
	`

// The sentinel-releases-snooze §4.3 statements. releaseFirstSeenQuery is step 0's read — a plain
// SELECT, no FOR UPDATE / FOR SHARE: reading first_seen with a lock at the top of the transaction
// would hold the project's hottest row for the whole transaction and invert the lock order every
// other side-table write follows (decision 4). Two concurrent first sightings both reading "absent"
// is harmless: the upsert's ON CONFLICT arbitrates and RETURNING inserted says who fans out.
// releaseUpsertQuery is step 4c: $3 = occ.CreatedAt (§3, decision 7 — the event's own clock,
// exactly as issues.first_seen/last_seen are), so first_seen = last_seen on the INSERT arm;
// GREATEST on last_seen mirrors issues.last_seen for out-of-order client timestamps.
//
// The INSERT arm clamps both to LEAST($3, now()) (decision 50, `release-first-seen-clock-guard`,
// which narrows decision 7 for `releases` only). Without it one event whose client clock runs a
// year ahead creates a (project, version) row whose first_seen is in the future: every `next`
// resolution of the project is then rewritten to that version (it is "first seen" after any
// resolved_at), and when the real next release arrives an occurrence in it reads
// rowR.first_seen < rowX.first_seen → not a regression — regression detection silently off until
// the clock catches up, on a row that is never swept (§12). A past/zero clock still creates an
// "oldest" row, which is harmless, so only the future side is clamped. now() is the transaction's
// start timestamp, so the clamp is stable across the statement and reads as "no later than this
// ingest". The DO UPDATE arm is the contract's, unchanged — but EXCLUDED.last_seen is the row
// proposed for insertion, i.e. the clamped value, so GREATEST can never move last_seen past now()
// either: a release's last_seen is "latest occurrence we stored, no later than this ingest", and
// nothing compares it against occurrence clocks (releases are never swept, §12) — unlike the
// affected-users rollup above, whose last_seen retention does compare. RETURNING first_seen hands
// the value ACTUALLY written to the caller, so the post-commit `next` rewrite (§4.3 "After commit")
// compares against the row, never against the unclamped occ.CreatedAt — the two differ exactly
// when the clamp fired.
const (
	releaseFirstSeenQuery = `SELECT first_seen FROM releases WHERE project_id = $1 AND version = $2`

	releaseUpsertQuery = `
		INSERT INTO releases (project_id, version, first_seen, last_seen, occurrence_count)
		VALUES ($1, $2, LEAST($3, now()), LEAST($3, now()), 1)
		ON CONFLICT (project_id, version) DO UPDATE SET
		   last_seen = GREATEST(releases.last_seen, EXCLUDED.last_seen),
		   occurrence_count = releases.occurrence_count + 1
		RETURNING (xmax = 0) AS inserted, first_seen
	`
)

// releaseRow is one step-0 read of `releases`: the row's first_seen, or absent — no occurrence of
// this project has carried the version before this transaction (a first sighting).
type releaseRow struct {
	present   bool
	firstSeen time.Time
}

// readReleaseRow runs releaseFirstSeenQuery on tx and maps pgx.ErrNoRows to an absent row. Any
// other error is the statement genuinely failing and is returned as such (the same 25P02 argument
// UpsertIssueWithOutcome's SELECT makes: falling through would replace the real cause).
func readReleaseRow(ctx context.Context, tx pgx.Tx, projectID, version string) (releaseRow, error) {
	var row releaseRow
	err := tx.QueryRow(ctx, releaseFirstSeenQuery, projectID, version).Scan(&row.firstSeen)
	switch {
	case err == nil:
		row.present = true
		return row, nil
	case errors.Is(err, pgx.ErrNoRows):
		return releaseRow{}, nil
	default:
		return releaseRow{}, fmt.Errorf("failed to read release project=%s version=%q: %w", projectID, version, err)
	}
}

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

// decideRegression is sentinel-releases-snooze §4.3's decision table, row for row, for an existing
// issue whose status is 'resolved': does this occurrence — carrying releaseVersion (R) — regress an
// issue resolved in resolvedInVersion (X)? It is pure: the two `releases` reads of step 0 are
// passed in as rowR / rowX (absent = no occurrence of this project has carried that version before
// this transaction), and resolvedAt is the row's resolved_at (nil = NULL). Releases are ordered by
// when the project first saw them, never by parsing the version (decision 1); isRegressionVersion
// survives only as the fallback for a version nobody ingested and for a first_seen tie (decision 3),
// unchanged — equal → yes, empty → yes.
//
//	X            R / rows                              regression?
//	'' / NULL    any                                   yes  (unchanged today)
//	any          ''                                    yes  (unchanged today)
//	next         resolved_at IS NULL                   yes  (decision 27: a pre-feature batch resolve has nothing to compare against)
//	next         rowR absent                           yes  (decision 6: a release first seen now is newer than the resolution)
//	next         rowR.first_seen > resolved_at         yes  (decision 6)
//	next         rowR.first_seen <= resolved_at        no   (decisions 6, 32: R existed when the resolution was made, in whichever environment)
//	a version    rowX absent                           isRegressionVersion(R, X)  (decision 3: typed, never ingested)
//	a version    rowX present, rowR absent             yes  (decision 14: a first sighting is newest)
//	a version    rowX present, R == X                  yes  (decision 13: the fix did not land)
//	a version    rowR.first_seen > rowX.first_seen     yes  (decision 1)
//	a version    rowR.first_seen < rowX.first_seen     no   (decision 1)
//	a version    equal first_seen, R != X              isRegressionVersion(R, X)  (decision 3: the tie)
func decideRegression(releaseVersion, resolvedInVersion string, resolvedAt *time.Time, rowR, rowX releaseRow) bool {
	if resolvedInVersion == "" || releaseVersion == "" {
		return true
	}
	if resolvedInVersion == "next" {
		if resolvedAt == nil || !rowR.present {
			return true
		}
		return rowR.firstSeen.After(*resolvedAt)
	}
	if !rowX.present {
		return isRegressionVersion(releaseVersion, resolvedInVersion)
	}
	if !rowR.present || releaseVersion == resolvedInVersion {
		return true
	}
	if rowR.firstSeen.After(rowX.firstSeen) {
		return true
	}
	if rowR.firstSeen.Before(rowX.firstSeen) {
		return false
	}
	return isRegressionVersion(releaseVersion, resolvedInVersion)
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
			// sentinel-releases-snooze §4.3: the same decision table StoreEvent uses, called with
			// rowR and rowX both ABSENT and resolved_at NULL — this method performs none of step 0's
			// reads on purpose. It is a test-only path (tests/integration/processor_store_test.go;
			// the service package no longer calls it), so the shared function is still exercised
			// without adding new SELECTs to a legacy path. Absent rows land an explicit version on
			// the fallback row (isRegressionVersion, unchanged) and `next` on the first-sighting row
			// (always a regression) — so this path can never return the two new outcomes.
			isRegressed = decideRegression(releaseVersion, existingResolvedInVersion, nil, releaseRow{}, releaseRow{})
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
//  0. (sentinel-releases-snooze §4.3) Two plain reads of `releases` — no lock, no new lock order
//     (decision 4): first_seen for the incoming version R (only when R <> ”), before step 1; and,
//     only after step 1's SELECT finds status = 'resolved' with an explicit resolved-in version X
//     (not ” and not 'next'), first_seen for X. Absent means a first sighting. Both are behind the
//     compile-time constant writeRelease (decision 11, release_write.go / release_write_off.go);
//     with it false decideRegression sees both absent and falls back to isRegressionVersion.
//  1. Read existing issue state — the SELECT also returns resolved_at, the three snooze columns,
//     count and the transaction's now(); then decide. A 'resolved' issue regresses per
//     decideRegression's table (§4.3; decision 1: ordered by first_seen, never by parsing the
//     version). An 'ignored' issue ENDS its snooze iff snoozed_until <= now() (the server's clock,
//     §3, decision 7) or (count + 1) - snooze_count_at >= snooze_occurrence_target (this occurrence
//     included) — the snooze arm: status → 'unresolved', the three columns NULL, count + 1, and one
//     'status_changed' activity row by 'sentinel-snooze' whose new_value carries `reason`, the three
//     PRE-null snooze values from the snapshot and the committed count from RETURNING (decisions 17,
//     18, 40). Otherwise upsert/update exactly as UpsertIssueWithOutcome did, but with RETURNING id
//     folded into ALL arms so the separate GetIssueIDByFingerprint round trip disappears; the
//     fresh-insert arm stamps first_release_version = NULLIF(R, ”) and the DO UPDATE arm never
//     touches it (the first release is the first release). ALL arms check rows affected: a folded
//     RETURNING id yielding pgx.ErrNoRows is a BUG (the DO UPDATE...WHERE predicate was narrowed, or
//     the plain UPDATE hit a concurrently-deleted issue), never a duplicate signal — it is surfaced
//     as an error, not stored=false.
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
//     4b. The occurrence bucket upsert (sentinel-issue-trend §4.3): one tx.Exec of
//     INSERT INTO issue_occurrence_buckets … ON CONFLICT (issue_id, bucket_start) DO UPDATE SET
//     count = count + 1, keyed on issue.ID and occ.CreatedAt.UTC().Truncate(time.Hour) computed in
//     Go (§3, decision 2 — never date_trunc on a timestamptz, which truncates in the session's
//     TimeZone). After step 4 and before the registry touch, so a deduplicated redelivery never
//     reaches it; NOT savepointed (decision 1); behind the compile-time constant
//     writeOccurrenceBucket (decision 20, bucket_write.go / bucket_write_off.go).
//     4c. The release upsert (sentinel-releases-snooze §4.3): one QueryRow of INSERT INTO releases …
//     ON CONFLICT (project_id, version) DO UPDATE SET last_seen = GREATEST(…), occurrence_count + 1
//     RETURNING (xmax = 0) AS inserted, first_seen, keyed on issue.ProjectID and R with
//     LEAST(occ.CreatedAt, now()) as the clock (§3, decision 7 narrowed by decision 50: a future
//     client clock is clamped to the transaction's now() on the INSERT arm). Only when R <> ”. After
//     step 4b and before the registry touch, so a deduplicated redelivery never reaches it (flow
//     I10); NOT savepointed (decision 5); behind the compile-time constant writeRelease (decision
//     11). `inserted` and the written first_seen are written back to occ.ReleaseInserted /
//     occ.ReleaseFirstSeen after commit — the post-commit `next` rewrite's signal and clock
//     (decision 15).
//  5. The project_environments registry touch (sentinel-project-environments §4.8), savepointed.
//     Then commit.
//
// Deadlock-freedom depends on this transaction acquiring its ONE contended lock (the issues row, via
// ON CONFLICT DO UPDATE / the plain UPDATE) FIRST and holding it to commit — do not add audit
// persistence, indexing, or any network I/O inside this transaction; move it after commit instead
// (D-d). Four writes are the DELIBERATE exceptions to that sentence, and all four were added knowing it:
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
//   - the occurrence bucket upsert of step 4b (sentinel-issue-trend decision 1) — the THIRD
//     deliberate exception, for the same reason as the second: a bucket that can move without an
//     occurrence behind it is the S18 defect again, on a chart. Written after commit, a NAK between
//     the occurrence commit and the bucket write would either lose the increment or, on retry, find
//     the occurrence deduplicated and never count it. Inside, both land or neither does. The "one
//     lock" property survives exactly as it does for the second exception: the issues row lock is
//     still taken FIRST and held to commit, so two deliveries for the same issue are serialised
//     before either touches a bucket row, two deliveries for different issues share no bucket row
//     (the key includes issue_id), and the retention sweep takes bucket rows only — it never locks
//     issues in that step — so ingest (issues → bucket) versus sweep (bucket only) has no cycle.
//     Cost, stated: every stored occurrence writes one more row version — its hour's bucket row —
//     on top of the issues row version it already writes; sentinel-issue-trend §13 M2 reports it.
//   - the release upsert of step 4c (sentinel-releases-snooze decision 5) — the FOURTH deliberate
//     exception, a first-class fact like the second and third: a release count that can move
//     without an occurrence behind it is the S18 defect again, on a new table. Written after
//     commit, a NAK between the occurrence commit and the release write would either lose the count
//     or, on retry, find the occurrence deduplicated and never count it. Inside, both land or
//     neither does. The "one lock" property survives as it does for the others: the issues row lock
//     is still taken FIRST (step 1) and held to commit; the releases row is taken AFTER every issues
//     write and every side-table write of this transaction, like the project_environments row is,
//     so ingest-vs-ingest has no cycle (X → (P,V) and Y → (P,V) serialise on (P,V) at the tail).
//     Step 0's reads are plain SELECTs and take nothing (decision 4). Cost, stated: this is a HOT
//     row — every occurrence of a project on its current release rewrites one releases row, the
//     same cardinality as the project_environments row step 5 already rewrites; sentinel-releases-
//     snooze §13 M1 reports it.
//
// The IssueOutcome return is the class of the row's status AFTER the write (sentinel-releases-snooze
// decision 29) and the service reads it: New from `inserted` (xmax = 0) on the upsert; Regressed
// from the regression arm; Unsnoozed from the snooze arm; Ignored / Existing from RETURNING status
// on the DO UPDATE arm — 'ignored' → Ignored (the service skips dispatchAlert, decision 16),
// 'unresolved' or 'resolved' → Existing. NEVER from the step-1 snapshot, which is a plain SELECT
// before the row lock and therefore best-effort: under a race it can be stale, while RETURNING
// reads the row this transaction just wrote. The duplicate path (stored = false) always reports
// IssueOutcomeExisting regardless of what the original delivery was.
func (s *pgStore) StoreEvent(ctx context.Context, issue *Issue, occ *ErrorOccurrence, releaseVersion string) (IssueOutcome, bool, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return IssueOutcomeExisting, false, err
	}
	defer tx.Rollback(ctx)

	// R is the incoming release (sentinel-releases-snooze §4.3: `R = occ.ReleaseVersion`, which is
	// the same wire value the service passes as releaseVersion — processor_service.go builds occ from
	// evt.ReleaseVersion and passes evt.ReleaseVersion here). Compared byte-for-byte, never trimmed or
	// normalised (§3). "" means no release: no step-0 read, NULL first_release_version, no step 4c.
	R := releaseVersion

	// --- 0. The release read for R (sentinel-releases-snooze §4.3, decisions 4 and 11) ---
	//
	// A plain SELECT before step 1 takes the issues row lock: no lock of its own, no new lock order.
	// The second read (rowX) waits until step 1's SELECT says whether there is an explicit X to read.
	var rowR, rowX releaseRow
	if writeRelease && R != "" {
		if rowR, err = readReleaseRow(ctx, tx, issue.ProjectID, R); err != nil {
			return IssueOutcomeExisting, false, err
		}
	}

	// --- 1. Issue upsert / regression bookkeeping (UpsertIssueWithOutcome's logic, folded) ---
	//
	// Widened (§4.3) to also return resolved_at (the `next` rows of decideRegression's table), the
	// three snooze columns and count (the snooze arm's decision and its activity payload's PRE-null
	// values), and now() — the transaction's server clock, so "snoozed_until <= now()" is judged on
	// the clock §3 names (a skewed client must not end a snooze early; decision 7) without a second
	// round trip. This SELECT takes no lock and is best-effort under a race — the outcome is never
	// derived from it (decision 29); only the decision and the payload's snapshot values are.

	var existingStatus, existingResolvedInVersion string
	var existingID string
	var existingResolvedAt, existingSnoozedUntil *time.Time
	var existingSnoozeTarget, existingSnoozeCountAt *int64
	var existingCount int64
	var txNow time.Time

	err = tx.QueryRow(ctx,
		`SELECT id, status, COALESCE(resolved_in_version, ''), resolved_at,
		        snoozed_until, snooze_occurrence_target, snooze_count_at, count, now()
		   FROM issues WHERE project_id = $1 AND environment = $2 AND fingerprint = $3`,
		issue.ProjectID, issue.Environment, issue.Fingerprint,
	).Scan(&existingID, &existingStatus, &existingResolvedInVersion, &existingResolvedAt,
		&existingSnoozedUntil, &existingSnoozeTarget, &existingSnoozeCountAt, &existingCount, &txNow)

	// Only "no such issue yet" is expected here; see UpsertIssueWithOutcome's identical comment for why
	// falling through on any other error would replace the real cause with a generic 25P02 abort.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return IssueOutcomeExisting, false, fmt.Errorf("failed to read existing issue for project=%s environment=%s fingerprint=%s: %w",
			issue.ProjectID, issue.Environment, issue.Fingerprint, err)
	}

	// No wasNewIssue here any more: New is derived from the upsert's own RETURNING (xmax = 0) AS
	// inserted, never from this pre-lock snapshot (decision 29; the doc comment's outcome rule).
	isRegressed := false
	// snoozeEnds / snoozeReason: the snooze arm's decision (§4.3, flows I8/I9/I14/I16). The date is
	// checked first, so when both conditions hold on the same occurrence reason is
	// 'snoozed_until_passed'. count + 1 is THIS occurrence included; snooze_count_at is non-NULL
	// whenever the target is (check_snooze_pair), the nil check is belt and braces.
	snoozeEnds := false
	snoozeReason := ""
	if err == nil {
		issue.ID = existingID
		switch existingStatus {
		case "resolved":
			// The second step-0 read: only for an explicit X that a `releases` row could exist for,
			// and only when there is an R to compare it with (decideRegression answers "yes" for
			// R == "" before it looks at either row, so the read would be wasted).
			if writeRelease && R != "" && existingResolvedInVersion != "" && existingResolvedInVersion != "next" {
				if rowX, err = readReleaseRow(ctx, tx, issue.ProjectID, existingResolvedInVersion); err != nil {
					return IssueOutcomeExisting, false, err
				}
			}
			// Beside UpsertIssueWithOutcome's call: the same pure table, this time fed step 0's rows.
			isRegressed = decideRegression(R, existingResolvedInVersion, existingResolvedAt, rowR, rowX)
		case "ignored":
			switch {
			case existingSnoozedUntil != nil && !existingSnoozedUntil.After(txNow):
				snoozeEnds, snoozeReason = true, "snoozed_until_passed"
			case existingSnoozeTarget != nil && existingSnoozeCountAt != nil &&
				(existingCount+1)-*existingSnoozeCountAt >= *existingSnoozeTarget:
				snoozeEnds, snoozeReason = true, "snooze_occurrences_reached"
			}
		}
	}

	// postWriteStatus is what the DO UPDATE arm's RETURNING status said — the outcome's source for
	// Ignored vs Existing (decision 29). Empty on every other arm.
	postWriteStatus := ""
	wasFreshInsert := false

	if isRegressed {
		// The three snooze columns are nulled here too, although the step-1 snapshot said
		// `resolved` and a resolved row carries no snooze (§4.2). The snapshot is UNLOCKED: a
		// dashboard PATCH {status: 'ignored', snoozeOccurrences: N} can commit between it and this
		// UPDATE, which then re-evaluates the row under READ COMMITTED and, with the columns left
		// alone, would set status = 'unresolved' beside a live snoozed_until — refused by
		// check_snooze_only_when_ignored (23514), which classifyStoreError makes Permanent: the
		// occurrence would be dead-lettered and never replayed (D14) for a race that, before this
		// feature, merely overwrote the ignore. Nulling them costs nothing on a genuinely resolved
		// row and keeps that pre-feature outcome: the regression wins, the snooze goes with the
		// status it belonged to. Pinned by TestStoreEvent_RegressionArmSurvivesConcurrentIgnoreWithSnooze.
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
				snoozed_until = NULL,
				snooze_occurrence_target = NULL,
				snooze_count_at = NULL,
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
	} else if snoozeEnds {
		// The snooze arm (sentinel-releases-snooze §4.3, decisions 17, 18, 40): the condition a
		// person parked this issue behind was met by this occurrence. status → 'unresolved', the
		// three snooze columns NULL in the SAME statement (check_snooze_only_when_ignored would
		// refuse anything else), count + 1, last_seen bumped — and NO regression column moves:
		// nothing regressed, a person parked it (decision 17). RETURNING count is the committed
		// value the activity row reports (600 in flow I9, not the snapshot's 599).
		query := `
			UPDATE issues SET
				status = 'unresolved',
				snoozed_until = NULL,
				snooze_occurrence_target = NULL,
				snooze_count_at = NULL,
				last_seen = GREATEST(issues.last_seen, $1),
				count = issues.count + 1
			WHERE id = $2
			RETURNING id, count
		`
		var updatedID string
		var committedCount int64
		if scanErr := tx.QueryRow(ctx, query, issue.LastSeen, issue.ID).Scan(&updatedID, &committedCount); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				// F-TX-7, exactly as the regression arm handles it: the row this UPDATE targeted no
				// longer exists (a concurrent retention DELETE won the race against the SELECT
				// above). A bug condition, never a duplicate — surface it.
				return IssueOutcomeExisting, false, fmt.Errorf(
					"snooze-ending update matched no row for issue id=%s (concurrently deleted?): %w",
					issue.ID, scanErr)
			}
			return IssueOutcomeExisting, false, scanErr
		}
		issue.ID = updatedID

		// The payload's three snooze values are the ones the row held BEFORE this statement nulled
		// them (the step-1 snapshot): null for the condition that was not set. snoozedUntil is
		// UTC, `Z`, whole seconds (decision 40; §3 Timestamps) — never the offset form; readers
		// parse it as an instant and never compare the strings.
		var snoozedUntilJSON, snoozeTargetJSON, snoozeCountAtJSON interface{}
		if existingSnoozedUntil != nil {
			snoozedUntilJSON = existingSnoozedUntil.UTC().Format(time.RFC3339)
		}
		if existingSnoozeTarget != nil {
			snoozeTargetJSON = *existingSnoozeTarget
		}
		if existingSnoozeCountAt != nil {
			snoozeCountAtJSON = *existingSnoozeCountAt
		}
		newValue, _ := json.Marshal(map[string]interface{}{
			"status":                 "unresolved",
			"reason":                 snoozeReason,
			"snoozedUntil":           snoozedUntilJSON,
			"snoozeOccurrenceTarget": snoozeTargetJSON,
			"snoozeCountAt":          snoozeCountAtJSON,
			"count":                  committedCount,
		})
		oldValue, _ := json.Marshal(map[string]string{"status": "ignored"})

		// 'status_changed', not a new event type (decision 18): the same row the dashboard writes
		// when a person changes the status, with `reason` on the way out as it carries
		// snoozedUntil / snoozeOccurrences on the way in. issue_activity's event_type CHECK is
		// untouched by this feature's migration.
		activityQuery := `
			INSERT INTO issue_activity (id, issue_id, actor_type, actor_id, event_type, old_value, new_value, created_at)
			VALUES (gen_random_uuid(), $1, 'system', 'sentinel-snooze', 'status_changed', $2, $3, NOW())
		`
		if _, err := tx.Exec(ctx, activityQuery, issue.ID, oldValue, newValue); err != nil {
			return IssueOutcomeExisting, false, err
		}
	} else {
		// N7a (A01/A06): RETURNING (xmax = 0) AS inserted gives an EXACT new-vs-existing signal
		// from the upsert statement itself, unlike wasNewIssue above (a pre-upsert SELECT in the
		// same tx — see the outcome caveat in this method's doc comment, and UpsertIssueWithOutcome's
		// equivalent SELECT). xmax=0 means this row's current version was created by THIS command
		// (a fresh INSERT), never by the DO UPDATE arm — so it cannot alias a concurrent-race
		// duplicate the way wasNewIssue can. RETURNING status is the same idea for the two
		// outcomes decision 29 adds: the status of the row THIS transaction just wrote, not the
		// snapshot's. first_release_version = NULLIF($10, '') on the INSERT arm only — the DO UPDATE
		// arm never touches it (sentinel-releases-snooze §4.3: the first release is the first release).
		query := `
			INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, status, first_seen, last_seen, count, first_release_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, NULLIF($10, ''))
			ON CONFLICT (project_id, environment, fingerprint)
			DO UPDATE SET
				last_seen = GREATEST(issues.last_seen, EXCLUDED.last_seen),
				count = issues.count + 1
			WHERE ` + issuesUpsertConflictPredicate + `
			RETURNING id, status, (xmax = 0) AS inserted
		`
		var returnedID string
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
			R,
		).Scan(&returnedID, &postWriteStatus, &wasFreshInsert); scanErr != nil {
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

	// --- 4b. Occurrence bucket upsert (sentinel-issue-trend §4.3, decisions 1, 2, 9, 20) ---
	//
	// The third deliberate in-transaction exception named in this method's doc comment. Same
	// position rules as step 4, for the same reasons: AFTER the duplicate rollback (flow I5), BEFORE
	// the registry touch, NOT savepointed (decision 1) — a genuine error here fails the transaction
	// and NATS redelivers. No RETURNING and no branch: nothing on issues is bumped for a new bucket.
	// bucket_start is computed in Go, in UTC (§3, decision 2): PostgreSQL's date_trunc on a
	// timestamptz truncates in the session's TimeZone, and a +05:30 session would put 20:29:59+05:30
	// in a different hour than 14:00Z. Behind the compile-time constant so §13 M2's baseline build
	// (-tags m2_nobucket) is this exact path minus this statement (decision 20).
	if writeOccurrenceBucket {
		if _, err := tx.Exec(ctx, occurrenceBucketUpsertQuery, issue.ID, occ.CreatedAt.UTC().Truncate(time.Hour)); err != nil {
			return IssueOutcomeExisting, false, fmt.Errorf("occurrence bucket upsert failed for issue id=%s: %w", issue.ID, err)
		}
	}

	// --- 4c. Release upsert (sentinel-releases-snooze §4.3, decisions 4, 5, 7, 11) ---
	//
	// The fourth deliberate in-transaction exception named in this method's doc comment. Same
	// position rules as 4 and 4b, for the same reasons: AFTER the duplicate rollback (flow I10 — a
	// redelivered (issue_id, event_id) never bumps a release), BEFORE the registry touch, NOT
	// savepointed (decision 5) — a genuine error here fails the event and NATS redelivers. Lock
	// order: the releases row is taken after every issues write and every side-table write of this
	// transaction, at the tail like the project_environments row, so ingest-vs-ingest serialises on
	// (project, version) and has no cycle. Only when R <> '' (no phantom release named ""; the
	// table's CHECK is the tripwire). Behind the compile-time constant so §13 M1's baseline build
	// (-tags m1_norelease) is this exact path minus this statement and step 0 (decision 11).
	releaseInserted := false
	var releaseFirstSeen time.Time
	if writeRelease && R != "" {
		if err := tx.QueryRow(ctx, releaseUpsertQuery, issue.ProjectID, R, occ.CreatedAt).Scan(&releaseInserted, &releaseFirstSeen); err != nil {
			return IssueOutcomeExisting, false, fmt.Errorf("release upsert failed for project=%s version=%q: %w", issue.ProjectID, R, err)
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

	// Written back only once the transaction is durable: a first sighting that did not commit is not
	// a first sighting, and the post-commit rewrite must never run on one (decision 15). The clock
	// travels with the signal: on the DO UPDATE arm RETURNING first_seen is the EXISTING row's value,
	// which is not this delivery's to fan out, so it is written back only when inserted (decision 50).
	occ.ReleaseInserted = releaseInserted
	if releaseInserted {
		occ.ReleaseFirstSeen = releaseFirstSeen
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

	// The outcome is the class of the post-write status (decision 29): each arm reports what it
	// wrote; the DO UPDATE arm reports what RETURNING status said. The step-1 snapshot's "no row
	// yet" is deliberately not consulted: under a first-insert race two deliveries can both read
	// that, but only the one whose upsert INSERTed (xmax = 0) is New.
	switch {
	case isRegressed:
		return IssueOutcomeRegressed, true, nil
	case snoozeEnds:
		return IssueOutcomeUnsnoozed, true, nil
	case wasFreshInsert:
		return IssueOutcomeNew, true, nil
	case postWriteStatus == "ignored":
		return IssueOutcomeIgnored, true, nil
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

// LookupSourcemap reads the one `sourcemaps` row for (projectID, releaseVersion, minifiedPath)
// through the composite UNIQUE index (§4.5). projectID MUST be ResolveProjectID's return -- the
// resolved tenant of the authenticated event, the value issue.ProjectID takes -- never anything
// read from a frame: the row is scoped by it, so project A's map is unreachable from project B's
// event whatever path B's frames name (contract risk surface `tenancy`; §11's tenancy case in
// tests/integration proves it against a seeded database). minifiedPath is the frame's file
// after symbolicate.NormalizePath, the same rule the upload route stored the row under.
//
// pgx.ErrNoRows is (nil, nil): a missing map is the stage's `no_map`, negatively cached, and not
// an error. Any other error is returned as-is (the stage reads it as a transient `fetch_error`).
func (s *pgStore) LookupSourcemap(ctx context.Context, projectID, releaseVersion, minifiedPath string) (*symbolicate.Sourcemap, error) {
	var sm symbolicate.Sourcemap
	err := s.db.QueryRow(ctx,
		`SELECT id::text, project_id::text, release_version, minified_path, storage_key, size_bytes, sha256
		   FROM sourcemaps
		  WHERE project_id = $1 AND release_version = $2 AND minified_path = $3`,
		projectID, releaseVersion, minifiedPath,
	).Scan(&sm.ID, &sm.ProjectID, &sm.ReleaseVersion, &sm.MinifiedPath, &sm.StorageKey, &sm.SizeBytes, &sm.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup sourcemap: %w", err)
	}
	return &sm, nil
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
