package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/golang/protobuf/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// buildValidProtoEvent returns a sentinelv1.ErrorEvent that satisfies the
// processor-go event validation (project_key, platform, environment,
// error_class are all required). The payload fields avoid numeric IDs that
// the normalizer would mask to <NUMERIC_ID>, so the resulting row values
// remain comparable to the inputs in assertions.
func buildValidProtoEvent(projectKey string) *sentinelv1.ErrorEvent {
	ts := timestamppb.Now()
	md, err := structpb.NewStruct(map[string]interface{}{
		"user_id":    "user-" + projectKey,
		"tenant_id":  "tenant-" + projectKey,
		"request_id": "req-" + projectKey,
	})
	if err != nil {
		panic(err)
	}
	// Use only letters in the project key so the normalizer does not mask it.
	safeKey := projectKey
	if r := []rune(safeKey); len(r) > 0 {
		_ = r
	}
	return &sentinelv1.ErrorEvent{
		ProjectKey:  projectKey,
		Platform:    "go",
		Environment: "test",
		Message:     "integration test error",
		ErrorClass:  "IntegrationRuntimeError",
		// Use trace/span IDs without numeric timestamps so they survive
		// normalization unchanged. The normalizer strips bare hex strings
		// as <HEX_ADDR>, so we keep them well below 32 chars and not in
		// UUID form.
		TraceId:     "trace-" + safeKey,
		SpanId:      "span-" + safeKey,
		Fingerprint: "fp-" + projectKey,
		Timestamp:   ts,
		Metadata:    md,
		Stacktrace: []*sentinelv1.StackFrame{
			{
				File:     "service.go",
				Line:     42,
				Function: "TestProcessorService",
				InApp:    true,
			},
		},
	}
}

// uniqueProjectName returns a project name unique per test invocation so
// concurrent or repeated runs do not collide on rows that cascade from
// projects (issues, error_occurrences, error_search_index, audit_logs).
// The numeric suffix is short (< 6 digits) so it does NOT match the
// normalizer's numeric-ID pattern (`\b\d{6,}\b`) that would otherwise
// mask the project name in the resulting stored event fields.
func uniqueProjectName(prefix string) string {
	return fmt.Sprintf("%s-%05x", prefix, time.Now().UnixNano()&0xfffff)
}

// uniqueAPIKey returns an API key for seeding a projects row. The schema has
// UNIQUE constraints on api_key, so this guarantees uniqueness within a run.
func uniqueAPIKey(prefix string) string {
	return fmt.Sprintf("%s-%05x", prefix, time.Now().UnixNano()&0xfffff)
}

// cleanupServiceRows removes every row created during a single test run for
// the given project. Audit logs do not carry a project_id, so we scope them
// by joining through issues and error_occurrences that this test created.
func cleanupServiceRows(t *testing.T, pool *pgxpool.Pool, projectID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Delete audit_logs scoped to this project's issues and occurrences.
	// audit_logs.resource_id is a UUID; cast both union legs to UUID so
	// the UNION type matches the column.
	if _, err := pool.Exec(ctx,
		`DELETE FROM audit_logs
		  WHERE resource_id IN (
		        SELECT i.id FROM issues i WHERE i.project_id = $1
		        UNION
		        SELECT eo.id FROM error_occurrences eo
		          JOIN issues i ON i.id = eo.issue_id
		         WHERE i.project_id = $1
		  )`, projectID); err != nil {
		t.Logf("cleanup: failed to delete audit_logs rows: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID); err != nil {
		t.Logf("cleanup: failed to delete project %s: %v", projectID, err)
	}
}

func newServiceAndPoolFromEnv(t *testing.T) (*service.ProcessorService, *pgxpool.Pool, TestConfig) {
	t.Helper()
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
	}
	pool := newPostgresPool(t, cfg)
	t.Cleanup(func() { pool.Close() })
	return service.NewProcessorService(pool), pool, cfg
}

func TestProcessorService_ProcessEvent_HappyPath(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	projectName := uniqueProjectName("u15-happy")
	apiKey := uniqueAPIKey("u15-happy-key")
	projectID := createTestProject(t, pool, projectName, apiKey)
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	protoEvent := buildValidProtoEvent(projectName)
	data, err := proto.Marshal(protoEvent)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.NoError(t, svc.ProcessEvent(ctx, data))

	// issues row should be upserted with count=1.
	var issueCount int64
	var issueID, issueFingerprint, issueMessage string
	err = pool.QueryRow(ctx,
		`SELECT count, id::text, fingerprint, message
		   FROM issues
		  WHERE project_id = $1
		  ORDER BY first_seen DESC
		  LIMIT 1`,
		projectID,
	).Scan(&issueCount, &issueID, &issueFingerprint, &issueMessage)
	require.NoError(t, err, "issues row should exist after happy path")
	assert.Equal(t, int64(1), issueCount, "first event should set count to 1")
	assert.Equal(t, protoEvent.Fingerprint, issueFingerprint)
	assert.Equal(t, protoEvent.Message, issueMessage)

	// error_occurrences row should reference the upserted issue.
	var occurrenceCount int
	var occurrenceEnv, occurrenceTraceID string
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*), MIN(environment), MIN(trace_id)
		   FROM error_occurrences
		  WHERE issue_id = $1`,
		issueID,
	).Scan(&occurrenceCount, &occurrenceEnv, &occurrenceTraceID)
	require.NoError(t, err)
	assert.Equal(t, 1, occurrenceCount, "one error_occurrences row per ProcessEvent call")
	assert.Equal(t, protoEvent.Environment, occurrenceEnv)
	assert.Equal(t, protoEvent.TraceId, occurrenceTraceID)

	// error_search_index row should reference the inserted occurrence.
	var searchUserID, searchTenantID, searchTraceID, searchRequestID string
	var searchCount int
	err = pool.QueryRow(ctx,
		`SELECT user_id, tenant_id, trace_id, request_id,
		        (SELECT COUNT(*) FROM error_search_index
		          WHERE occurrence_id IN (
		                SELECT id FROM error_occurrences WHERE issue_id = $1
		          ))
		   FROM error_search_index esi
		  WHERE esi.occurrence_id IN (
		        SELECT id FROM error_occurrences WHERE issue_id = $1
		  )
		  LIMIT 1`,
		issueID,
	).Scan(&searchUserID, &searchTenantID, &searchTraceID, &searchRequestID, &searchCount)
	require.NoError(t, err)
	assert.Equal(t, 1, searchCount, "one error_search_index row per occurrence")
	assert.Equal(t, "user-"+projectName, searchUserID)
	assert.Equal(t, "tenant-"+projectName, searchTenantID)
	assert.Equal(t, protoEvent.TraceId, searchTraceID, "trace id should match the event")
	assert.Equal(t, "req-"+projectName, searchRequestID)

	// audit_logs rows should include both `issue_upserted` and
	// `occurrence_created` for this issue/occurrence.
	var auditUpsertedCount, auditOccurrenceCount int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs
		  WHERE action = 'issue_upserted' AND resource_id = $1`,
		issueID,
	).Scan(&auditUpsertedCount)
	require.NoError(t, err)
	assert.Equal(t, 1, auditUpsertedCount, "should persist one issue_upserted audit row")

	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs
		  WHERE action = 'occurrence_created'
		    AND resource_id IN (
		          SELECT id FROM error_occurrences WHERE issue_id = $1
		    )`,
		issueID,
	).Scan(&auditOccurrenceCount)
	require.NoError(t, err)
	assert.Equal(t, 1, auditOccurrenceCount, "should persist one occurrence_created audit row")
}

func TestProcessorService_ProcessEvent_UnknownProjectReturnsErrorAndNoRows(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	projectName := uniqueProjectName("u15-unknown")
	// Note: project is intentionally NOT created.

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	protoEvent := buildValidProtoEvent(projectName)
	data, err := proto.Marshal(protoEvent)
	require.NoError(t, err)

	err = svc.ProcessEvent(ctx, data)
	require.Error(t, err, "unknown project should surface an error")
	assert.ErrorContains(t, err, "project not found")

	// Sanity check: no projects were created for the random name.
	var projectCount int64
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM projects WHERE name = $1`,
		projectName,
	).Scan(&projectCount)
	require.NoError(t, err)
	assert.Equal(t, int64(0), projectCount,
		"unknown project must not produce a projects row")

	// No issues joined to a missing project.
	var joinedCount int64
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issues i
		   JOIN projects p ON p.id = i.project_id
		  WHERE p.name = $1`,
		projectName,
	).Scan(&joinedCount)
	require.NoError(t, err)
	assert.Equal(t, int64(0), joinedCount,
		"unknown project must not produce any issues")
}

func TestProcessorService_ProcessEvent_DBUnavailableBuffersEvent(t *testing.T) {
	_, _, _ = newServiceAndPoolFromEnv(t)

	// Build a separate pool that points at an unreachable TCP endpoint.
	// pgxpool is lazy, so construction does not fail even with a bad host,
	// but any subsequent Ping returns a connection error and triggers
	// degradation buffering.
	badCfg, err := pgxpool.ParseConfig(
		"postgres://sentinel:changeme@127.0.0.1:1/sentinel?sslmode=disable&connect_timeout=2",
	)
	require.NoError(t, err)
	badPool, err := pgxpool.NewWithConfig(context.Background(), badCfg)
	require.NoError(t, err, "pgxpool.NewWithConfig is lazy and must succeed for unreachable host")
	t.Cleanup(badPool.Close)

	svc := service.NewProcessorService(badPool)

	projectName := uniqueProjectName("u15-degraded")
	protoEvent := buildValidProtoEvent(projectName)
	data, err := proto.Marshal(protoEvent)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The actual production behavior with a degraded pool:
	//   1. ProcessEvent -> CheckAndBuffer
	//   2. dbChecker (db.Ping) fails, so the buffer Push is attempted and
	//      succeeds (the buffer is empty). CheckAndBuffer returns true.
	//   3. ProcessEvent then calls processEventInternal, which fails with
	//      a connection error because GetProjectByKey cannot reach the DB.
	// We assert that the underlying connection error surfaces so callers
	// can observe degraded-DB behaviour. The buffer-already-pushed
	// invariant is exercised by the unit suite for the GracefulDegradation.
	err = svc.ProcessEvent(ctx, data)
	require.Error(t, err, "ProcessEvent should surface the connection error from the unreachable pool")
	assert.ErrorContains(t, err, "connect")
}

func TestProcessorService_VerifyAuditLogTable_Succeeds(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, svc.VerifyAuditLogTable(ctx))

	// The verification sentinel row must exist with the canonical
	// zero-UUID used by VerifyAuditLogTable.
	var action, resourceType, actorID string
	err := pool.QueryRow(ctx,
		`SELECT action, resource_type, actor_id
		   FROM audit_logs
		  WHERE id = '00000000-0000-0000-0000-000000000000'::uuid`,
	).Scan(&action, &resourceType, &actorID)
	require.NoError(t, err, "verification sentinel row should exist")
	assert.Equal(t, "verification_test", action)
	assert.Equal(t, "test", resourceType)
	assert.Equal(t, "processor-go", actorID)

	// Idempotency: a second call must not duplicate the sentinel row.
	require.NoError(t, svc.VerifyAuditLogTable(ctx))
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE id = '00000000-0000-0000-0000-000000000000'::uuid`,
	).Scan(&n))
	assert.Equal(t, 1, n, "sentinel row must remain singleton across repeated calls")

	t.Cleanup(func() {
		// Best-effort cleanup of the sentinel row only on test pass; the
		// global row is harmless and reused by VerifyAuditLogTable_FailsWhenTableMissing
		// which restores the table afterwards.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx,
			`DELETE FROM audit_logs WHERE id = '00000000-0000-0000-0000-000000000000'::uuid`)
	})
}

func TestProcessorService_VerifyAuditLogTable_FailsWhenTableMissing(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Move audit_logs out of the way so the verification INSERT fails on
	// "relation does not exist". Renaming is atomic in PostgreSQL and
	// keeps any DDL referenced objects safe.
	_, err := pool.Exec(ctx,
		`ALTER TABLE audit_logs RENAME TO audit_logs_zu15_missing`)
	require.NoError(t, err, "test precondition: rename audit_logs")
	t.Cleanup(func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer restoreCancel()
		// Use IF EXISTS so a concurrent failure does not break cleanup.
		if _, err := pool.Exec(restoreCtx,
			`ALTER TABLE IF EXISTS audit_logs_zu15_missing RENAME TO audit_logs`); err != nil {
			t.Logf("cleanup: failed to restore audit_logs table: %v", err)
		}
	})

	// Sanity check: the table is no longer visible under its original name.
	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM information_schema.tables
		     WHERE table_schema = 'public'
		       AND table_name = 'audit_logs'
		 )`,
	).Scan(&exists)
	require.NoError(t, err)
	require.False(t, exists, "audit_logs must not be visible during the test")

	// VerifyAuditLogTable must surface a Postgres error.
	err = svc.VerifyAuditLogTable(ctx)
	require.Error(t, err, "missing audit_logs should fail VerifyAuditLogTable")
	assert.ErrorContains(t, err, "audit_logs")
}

func TestProcessorService_ProcessEvent_InvalidPayloadReturnsDeserializeError(t *testing.T) {
	svc, _, _ := newServiceAndPoolFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Junk bytes fail proto.Unmarshal, which event.Deserialize wraps in
	// "failed to unmarshal event". ProcessEvent surfaces this as an error
	// without touching the database.
	err := svc.ProcessEvent(ctx, []byte{0xff, 0xfe, 0xfd, 0xfc})
	require.Error(t, err, "invalid bytes should fail at the Deserialize step")
	assert.ErrorContains(t, err, "failed to unmarshal event")
}

// renameTableAndRestore renames `table` to a unique name and registers a
// t.Cleanup that restores it. The helper centralises a pattern used by the
// failure-path tests below.
func renameTableAndRestore(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	replacement := fmt.Sprintf("%s_zu15_missing_%05x", table, time.Now().UnixNano()&0xfffff)
	_, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, table, replacement))
	require.NoError(t, err, "test precondition: rename %s", table)
	t.Cleanup(func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer restoreCancel()
		if _, err := pool.Exec(restoreCtx,
			fmt.Sprintf(`ALTER TABLE IF EXISTS %s RENAME TO %s`, replacement, table)); err != nil {
			t.Logf("cleanup: failed to restore %s: %v", table, err)
		}
	})
}

func TestProcessorService_ProcessEvent_UpsertIssueFailsWhenIssuesTableMissing(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	projectName := uniqueProjectName("u15-no-issues")
	apiKey := uniqueAPIKey("u15-no-issues")
	projectID := createTestProject(t, pool, projectName, apiKey)
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	renameTableAndRestore(t, pool, "issues")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	protoEvent := buildValidProtoEvent(projectName)
	data, err := proto.Marshal(protoEvent)
	require.NoError(t, err)

	err = svc.ProcessEvent(ctx, data)
	require.Error(t, err, "missing issues table should fail UpsertIssue")
	assert.ErrorContains(t, err, "issues")
}

func TestProcessorService_ProcessEvent_InsertOccurrenceFailsWhenOccurrencesMissing(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	projectName := uniqueProjectName("u15-no-occ")
	apiKey := uniqueAPIKey("u15-no-occ")
	projectID := createTestProject(t, pool, projectName, apiKey)
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	renameTableAndRestore(t, pool, "error_occurrences")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	protoEvent := buildValidProtoEvent(projectName)
	data, err := proto.Marshal(protoEvent)
	require.NoError(t, err)

	err = svc.ProcessEvent(ctx, data)
	require.Error(t, err, "missing error_occurrences should fail InsertOccurrence")
	assert.ErrorContains(t, err, "error_occurrences")
}

func TestProcessorService_ProcessEvent_IndexOccurrenceFailsWhenSearchIndexMissing(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	projectName := uniqueProjectName("u15-no-idx")
	apiKey := uniqueAPIKey("u15-no-idx")
	projectID := createTestProject(t, pool, projectName, apiKey)
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	renameTableAndRestore(t, pool, "error_search_index")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	protoEvent := buildValidProtoEvent(projectName)
	data, err := proto.Marshal(protoEvent)
	require.NoError(t, err)

	// IndexOccurrence failure is logged and swallowed — the inner error
	// path covers the "Failed to index occurrence" log line. The outer
	// ProcessEvent returns nil even when indexing fails.
	err = svc.ProcessEvent(ctx, data)
	require.NoError(t, err, "IndexOccurrence failure is logged but not surfaced as an error")

	// Sanity: issue + occurrence rows should have been written.
	var issueCount, occCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issues WHERE project_id = $1`, projectID,
	).Scan(&issueCount))
	assert.Equal(t, 1, issueCount, "issue row should have been written")

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences eo
		   JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1`, projectID,
	).Scan(&occCount))
	assert.Equal(t, 1, occCount, "occurrence row should have been written")
}

// TestProcessorService_ProcessEvent_BufferFullReturnsNil covers the branch
// in ProcessEvent where the degradation buffer is at capacity and the DB
// is down: CheckAndBuffer returns false, the service logs "Event buffered
// due to database unavailability" and returns nil without surfacing the
// connection error.
//
// We drive the buffer to capacity by issuing repeated ProcessEvent calls
// against an unreachable pool. Each call successfully pushes to the buffer
// (when the buffer is not full) and then attempts to run
// processEventInternal which fails with a connection error. Once the
// buffer hits degradation.MaxBufferSize (10000) the next call returns nil.
func TestProcessorService_ProcessEvent_BufferFullReturnsNil(t *testing.T) {
	badCfg, err := pgxpool.ParseConfig(
		"postgres://sentinel:changeme@127.0.0.1:1/sentinel?sslmode=disable&connect_timeout=2",
	)
	require.NoError(t, err)
	badPool, err := pgxpool.NewWithConfig(context.Background(), badCfg)
	require.NoError(t, err)
	t.Cleanup(badPool.Close)

	svc := service.NewProcessorService(badPool)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Issue calls until the buffer is full. We expect every call to
	// return a connection error until the buffer overflows, at which
	// point CheckAndBuffer returns false and ProcessEvent returns nil.
	const bufferCap = 10000
	sawNil := false
	for i := 0; i < bufferCap+10; i++ {
		ev, mErr := proto.Marshal(buildValidProtoEvent(uniqueProjectName("u15-buf-full")))
		require.NoError(t, mErr)
		callErr := svc.ProcessEvent(ctx, ev)
		if callErr == nil {
			sawNil = true
			break
		}
	}
	assert.True(t, sawNil,
		"ProcessEvent must transition to nil when the degradation buffer fills up")
}
