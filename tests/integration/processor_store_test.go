package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uniqueProjectName returns a project name that is unique per test invocation
// so concurrent runs of the test suite do not collide on the same project row.
func uniqueProjectNameU13(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-proj-u13-%d", time.Now().UnixNano())
}

// uniqueAPIKey returns a sufficiently long API key value guaranteed to be
// unique for the duration of a single test run.
func uniqueAPIKeyU13(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-api-key-u13-%d", time.Now().UnixNano())
}

// seedProject inserts a project row and returns its generated ID. It also
// registers a cleanup that removes the project (and via ON DELETE CASCADE,
// every row that references it).
func seedProjectU13(t *testing.T, pool *pgxpool.Pool) (projectID string, projectName string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	projectName = uniqueProjectNameU13(t)
	apiKey := uniqueAPIKeyU13(t)

	err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'))
		 RETURNING id::text`,
		projectName, apiKey, apiKey,
	).Scan(&projectID)
	require.NoError(t, err, "seed project")

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM projects WHERE id = $1`, projectID); err != nil {
			t.Logf("cleanup project %s failed: %v", projectID, err)
		}
	})

	return projectID, projectName
}

// postgresConfigFromTest resolves the Postgres connection settings via testcontainers.Setup.
func postgresConfigFromTest(t *testing.T) (host, port, user, password, db string) {
	t.Helper()
	env := tc.Setup(t, tc.WithResources(tc.PostgresResource), tc.WithMigrations(true))
	if env.PGConfig.Host == "" {
		t.Skip("PostgreSQL not available")
	}
	return env.PGConfig.Host, env.PGConfig.Port, env.PGConfig.User, env.PGConfig.Password, env.PGConfig.DB
}

// storeFromTest builds a fresh pgxpool.Pool and constructs an IssueStore backed
// by that pool. The pool is closed automatically when the test ends.
func storeFromTest(t *testing.T) (store.IssueStore, *pgxpool.Pool) {
	t.Helper()

	host, port, user, password, db := postgresConfigFromTest(t)

	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		user, password, host, port, db,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx))

	t.Cleanup(pool.Close)

	return store.NewStore(pool), pool
}

func TestStorePackage_UpsertIssue_InsertsNewIssue(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	issue := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: fmt.Sprintf("fp-insert-%d", time.Now().UnixNano()),
		Message:     "first occurrence",
		ErrorClass:  "UpsertInsertError",
		Status:      "open",
		FirstSeen:   time.Now().UTC(),
		LastSeen:    time.Now().UTC(),
	}

	require.NoError(t, s.UpsertIssue(ctx, issue))

	// Verify the row was inserted with count = 1.
	var (
		gotID     string
		gotCount  int64
		gotStatus string
	)
	err := pool.QueryRow(ctx,
		`SELECT id::text, count, status FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, issue.Fingerprint,
	).Scan(&gotID, &gotCount, &gotStatus)
	require.NoError(t, err)
	assert.Equal(t, issue.ID, gotID, "inserted issue ID should match")
	assert.Equal(t, int64(1), gotCount, "first insert should set count to 1")
	assert.Equal(t, "open", gotStatus)
}

func TestStorePackage_UpsertIssue_DuplicateIncrementsCountAndUpdatesLastSeen(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fingerprint := fmt.Sprintf("fp-upsert-%d", time.Now().UnixNano())

	first := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: fingerprint,
		Message:     "first error",
		ErrorClass:  "UpsertDuplicateError",
		Status:      "open",
		FirstSeen:   time.Now().UTC().Add(-1 * time.Hour),
		LastSeen:    time.Now().UTC().Add(-1 * time.Hour),
	}
	require.NoError(t, s.UpsertIssue(ctx, first))

	var (
		firstID    string
		firstCount int64
		firstSeen  time.Time
		lastSeen1  time.Time
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text, count, first_seen, last_seen FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, fingerprint,
	).Scan(&firstID, &firstCount, &firstSeen, &lastSeen1))
	require.Equal(t, int64(1), firstCount)

	// A second call must increment count and update last_seen (using GREATEST).
	second := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: fingerprint,
		Message:     "second error",
		ErrorClass:  "UpsertDuplicateError",
		Status:      "open",
		FirstSeen:   time.Now().UTC(),
		LastSeen:    time.Now().UTC(),
	}
	require.NoError(t, s.UpsertIssue(ctx, second))

	var (
		secondID    string
		secondCount int64
		secondSeen  time.Time
		lastSeen2   time.Time
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text, count, first_seen, last_seen FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, fingerprint,
	).Scan(&secondID, &secondCount, &secondSeen, &lastSeen2))

	assert.Equal(t, firstID, secondID, "upsert must keep the original issue ID")
	assert.Equal(t, int64(2), secondCount, "count should increment by 1 on duplicate")
	assert.Equal(t, firstSeen, secondSeen, "first_seen must be preserved on conflict")
	assert.True(t, !lastSeen2.Before(lastSeen1), "last_seen should advance (or stay equal) after duplicate")
}

func TestStorePackage_UpsertIssue_DuplicateWithOlderLastSeenKeepsGreatest(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fingerprint := fmt.Sprintf("fp-greatest-%d", time.Now().UnixNano())
	now := time.Now().UTC()

	first := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: fingerprint,
		Message:     "first",
		ErrorClass:  "GreatestError",
		Status:      "open",
		FirstSeen:   now,
		LastSeen:    now,
	}
	require.NoError(t, s.UpsertIssue(ctx, first))

	// A duplicate call with an older last_seen must NOT regress last_seen.
	older := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: fingerprint,
		Message:     "stale duplicate",
		ErrorClass:  "GreatestError",
		Status:      "open",
		FirstSeen:   now.Add(-2 * time.Hour),
		LastSeen:    now.Add(-2 * time.Hour),
	}
	require.NoError(t, s.UpsertIssue(ctx, older))

	var lastSeen time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, fingerprint,
	).Scan(&lastSeen))

	// Compare against the recorded first-seen timestamp, which equals the
	// last_seen we initially wrote.
	assert.WithinDuration(t, now, lastSeen, time.Second,
		"last_seen should not regress on duplicate insert (uses GREATEST)")
}

func TestStorePackage_InsertOccurrence_InsertsRow(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Seed an issue so the foreign key reference is valid.
	issue := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: fmt.Sprintf("fp-occ-%d", time.Now().UnixNano()),
		Message:     "occ insert",
		ErrorClass:  "OccurrenceInsertError",
		Status:      "open",
		FirstSeen:   time.Now().UTC(),
		LastSeen:    time.Now().UTC(),
	}
	require.NoError(t, s.UpsertIssue(ctx, issue))

	stacktrace := json.RawMessage(`[{"file":"main.go","line":1,"function":"main"}]`)
	metadata := json.RawMessage(`{"k":"v"}`)

	occ := &store.ErrorOccurrence{
		ID:          uuid.New().String(),
		IssueID:     issue.ID,
		Environment: "test",
		Platform:    "go",
		Stacktrace:  stacktrace,
		Metadata:    metadata,
		TraceID:     "trace-occ-u13",
		SpanID:      "span-occ-u13",
		CreatedAt:   time.Now().UTC(),
	}

	require.NoError(t, s.InsertOccurrence(ctx, occ))

	var (
		gotID       string
		gotEnv      string
		gotPlatform string
		gotTraceID  string
		gotSpanID   string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text, environment, platform, trace_id, span_id FROM error_occurrences WHERE id = $1`,
		occ.ID,
	).Scan(&gotID, &gotEnv, &gotPlatform, &gotTraceID, &gotSpanID))

	assert.Equal(t, occ.ID, gotID)
	assert.Equal(t, "test", gotEnv)
	assert.Equal(t, "go", gotPlatform)
	assert.Equal(t, "trace-occ-u13", gotTraceID)
	assert.Equal(t, "span-occ-u13", gotSpanID)
}

func TestStorePackage_GetProjectByKey_ReturnsID(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, projectName := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := s.GetProjectByKey(ctx, projectName)
	require.NoError(t, err)
	assert.Equal(t, projectID, got)
}

func TestStorePackage_GetProjectByKey_UnknownReturnsError(t *testing.T) {
	s, _ := storeFromTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	unknown := fmt.Sprintf("missing-proj-%d", time.Now().UnixNano())
	got, err := s.GetProjectByKey(ctx, unknown)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "project not found")
}

func TestStorePackage_GetIssueIDByFingerprint_ReturnsID(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fingerprint := fmt.Sprintf("fp-get-%d", time.Now().UnixNano())
	issue := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: fingerprint,
		Message:     "get by fingerprint",
		ErrorClass:  "GetByFingerprintError",
		Status:      "open",
		FirstSeen:   time.Now().UTC(),
		LastSeen:    time.Now().UTC(),
	}
	require.NoError(t, s.UpsertIssue(ctx, issue))

	got, err := s.GetIssueIDByFingerprint(ctx, projectID, fingerprint)
	require.NoError(t, err)
	assert.Equal(t, issue.ID, got)
}

func TestStorePackage_GetIssueIDByFingerprint_UnknownReturnsError(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := s.GetIssueIDByFingerprint(ctx, projectID, "fp-does-not-exist")
	require.Error(t, err)
	assert.Empty(t, got)
}

func TestStorePackage_PersistAuditLog_InsertsRow(t *testing.T) {
	s, pool := storeFromTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	auditID := uuid.New().String()
	resourceID := uuid.New().String()
	metadata := json.RawMessage(`{"src":"u13","ok":true}`)

	entry := &store.AuditLog{
		ID:           auditID,
		Action:       "u13.test.insert",
		ResourceType: "test",
		ResourceID:   &resourceID,
		ActorID:      "actor-u13",
		Metadata:     metadata,
	}

	require.NoError(t, s.PersistAuditLog(ctx, entry))

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM audit_logs WHERE id = $1`, auditID); err != nil {
			t.Logf("cleanup audit log %s failed: %v", auditID, err)
		}
	})

	var (
		gotAction       string
		gotResourceType string
		gotResourceID   *string
		gotActorID      string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT action, resource_type, resource_id::text, actor_id FROM audit_logs WHERE id = $1`,
		auditID,
	).Scan(&gotAction, &gotResourceType, &gotResourceID, &gotActorID))

	assert.Equal(t, "u13.test.insert", gotAction)
	assert.Equal(t, "test", gotResourceType)
	require.NotNil(t, gotResourceID)
	assert.Equal(t, resourceID, *gotResourceID)
	assert.Equal(t, "actor-u13", gotActorID)
}

func TestStorePackage_PersistAuditLog_FailureIncrementsCounter(t *testing.T) {
	s, _ := storeFromTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	before := store.GetAuditPersistFailureCount()

	// Force a failure: a malformed UUID violates the column type. pgx will
	// return an error and the implementation must increment the counter.
	entry := &store.AuditLog{
		ID:           "not-a-uuid",
		Action:       "u13.test.failure",
		ResourceType: "test",
		ActorID:      "actor-u13",
		Metadata:     json.RawMessage(`{}`),
	}

	err := s.PersistAuditLog(ctx, entry)
	require.Error(t, err, "malformed UUID must surface as an error")

	after := store.GetAuditPersistFailureCount()
	assert.Equal(t, before+1, after,
		"PersistAuditLog must increment GetAuditPersistFailureCount on failure")
}
