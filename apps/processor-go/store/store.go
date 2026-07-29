package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
	UpsertIssueWithOutcome(ctx context.Context, issue *Issue, releaseVersion string) (IssueOutcome, error)
	InsertOccurrence(ctx context.Context, occ *ErrorOccurrence) error
	PersistAuditLog(ctx context.Context, log *AuditLog) error
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
			WHERE issues.fingerprint = EXCLUDED.fingerprint
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
