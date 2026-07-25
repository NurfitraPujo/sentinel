package store

import (
	"context"
	"encoding/json"
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
	GetIssueIDByFingerprint(ctx context.Context, projectID, fingerprint string) (string, error)
}

// CommandStore defines the "Write" side of the Issue store.
type CommandStore interface {
	UpsertIssue(ctx context.Context, issue *Issue, releaseVersion string) error
	InsertOccurrence(ctx context.Context, occ *ErrorOccurrence) error
	PersistAuditLog(ctx context.Context, log *AuditLog) error
}

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

func (s *pgStore) UpsertIssue(ctx context.Context, issue *Issue, releaseVersion string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Read current issue state for regression evaluation if issue exists
	var existingStatus, existingResolvedInVersion string
	var existingID string

	err = tx.QueryRow(ctx,
		"SELECT id, status, COALESCE(resolved_in_version, '') FROM issues WHERE project_id = $1 AND fingerprint = $2",
		issue.ProjectID, issue.Fingerprint,
	).Scan(&existingID, &existingStatus, &existingResolvedInVersion)

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
			return err
		}

		activityMeta, _ := json.Marshal(map[string]string{
			"releaseVersion":          releaseVersion,
			"previousResolvedVersion": existingResolvedInVersion,
		})

		activityQuery := `
			INSERT INTO issue_activity (id, issue_id, actor_type, actor_id, event_type, metadata, created_at)
			VALUES (gen_random_uuid(), $1, 'system', 'sentinel-regression-detector', 'regressed', $2, NOW())
		`
		if _, err := tx.Exec(ctx, activityQuery, issue.ID, activityMeta); err != nil {
			return err
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
			return err
		}
	}

	return tx.Commit(ctx)
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
