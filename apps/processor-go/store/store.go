package store

import (
	"context"
	"encoding/json"
	"fmt"
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
	UpsertIssue(ctx context.Context, issue *Issue) error
	InsertOccurrence(ctx context.Context, occ *ErrorOccurrence) error
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
	ID          string
	ProjectID   string
	Fingerprint string
	Message     string
	ErrorClass  string
	Status      string
	FirstSeen   time.Time
	LastSeen    time.Time
	Count       int64
}

type ErrorOccurrence struct {
	ID          string
	IssueID     string
	Environment string
	Platform    string
	Stacktrace  json.RawMessage
	Metadata    json.RawMessage
	TraceID     string
	SpanID      string
	CreatedAt   time.Time
}

func (s *pgStore) UpsertIssue(ctx context.Context, issue *Issue) error {
	query := `
		INSERT INTO issues (id, project_id, fingerprint, message, error_class, status, first_seen, last_seen, count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)
		ON CONFLICT (project_id, fingerprint) 
		DO UPDATE SET 
			last_seen = GREATEST(issues.last_seen, EXCLUDED.last_seen),
			count = issues.count + 1
		WHERE issues.fingerprint = EXCLUDED.fingerprint
	`

	_, err := s.db.Exec(ctx, query,
		issue.ID,
		issue.ProjectID,
		issue.Fingerprint,
		issue.Message,
		issue.ErrorClass,
		issue.Status,
		issue.FirstSeen,
		issue.LastSeen,
	)

	return err
}

func (s *pgStore) InsertOccurrence(ctx context.Context, occ *ErrorOccurrence) error {
	query := `
		INSERT INTO error_occurrences (id, issue_id, environment, platform, stacktrace, metadata, trace_id, span_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	_, err := s.db.Exec(ctx, query,
		occ.ID,
		occ.IssueID,
		occ.Environment,
		occ.Platform,
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
