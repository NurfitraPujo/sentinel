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

// TestProcessorService_ProcessEvent_DBUnavailableBuffersEvent's name is
// historical (kept so `-run` filters used elsewhere in docs/scripts keep
// matching): there is no in-process buffer anymore. See
// apps/processor-go/degradation/buffer.go's package doc comment — the
// buffer was removed, deliberately, in favor of D10's NATS bounded-retry/DLQ
// durability (docs/memory/DECISIONS.md D10). Re-verified 2026-07-29 against
// a full SENTINEL_E2E run: this test previously asserted the OLD behavior
// (CheckAndBuffer returns true even when down, so processEventInternal ran
// anyway and surfaced a raw "connect" error) and failed once that code path
// was removed — degradation.Evaluate now short-circuits before any DB call
// is attempted at all, so no connection error is ever produced here.
func TestProcessorService_ProcessEvent_DBUnavailableBuffersEvent(t *testing.T) {
	_, _, _ = newServiceAndPoolFromEnv(t)

	// Build a separate pool that points at an unreachable TCP endpoint.
	// pgxpool is lazy, so construction does not fail even with a bad host,
	// but any subsequent Ping (via degradation.Evaluate's dbChecker) reports
	// the database as unavailable.
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

	// Current production behavior with an unreachable pool:
	//   1. ProcessEvent -> degradation.Evaluate
	//   2. dbChecker (db.Ping) fails -> Evaluate returns StatusUnavailable
	//      without ever attempting processEventInternal.
	//   3. ProcessEvent returns the fixed sentinel error below so the caller
	//      (the NATS subscriber, in production) lets D10 redeliver it.
	// This is a deliberate behavior change from the prior "buffer, then try
	// anyway" design: the DB is never touched for an event evaluated while
	// unavailable, so no raw connection error is produced or expected here.
	err = svc.ProcessEvent(ctx, data)
	require.Error(t, err, "ProcessEvent must return an error so the caller (NATS) retries instead of acking")
	assert.ErrorContains(t, err, "database unavailable")
}

func TestProcessorService_VerifyAuditLogTable_Succeeds(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var before int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&before))

	require.NoError(t, svc.VerifyAuditLogTable(ctx))
	require.NoError(t, svc.VerifyAuditLogTable(ctx), "verification must be repeatable")

	// The check must WRITE NOTHING. It used to INSERT a fixed all-zero-UUID row with action
	// 'verification_test', and this test asserted that row existed — so the test was pinning the
	// implementation detail (a sentinel row) rather than the requirement (audit_logs is present and has
	// the columns this service needs). Permanent fabricated rows in an audit trail are worse than no
	// check at all: the table's entire value is that everything in it really happened.
	var after int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&after))
	assert.Equal(t, before, after, "VerifyAuditLogTable must not write to audit_logs")

	var sentinels int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action = 'verification_test'`).Scan(&sentinels))
	assert.Zero(t, sentinels, "the old 'verification_test' sentinel row must not be written any more")
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

// TestProcessorService_ProcessEvent_NeverSilentlyDropsWhenDBUnavailable
// replaces the former TestProcessorService_ProcessEvent_BufferFullReturnsNil.
//
// That test drove ProcessEvent to buffer capacity (degradation.MaxBufferSize
// = 10000) expecting the buffer-full branch to return nil — a silent drop
// the DECISIONS.md graceful-degradation entry and BUGS.md both documented as
// "real, silent data loss reported as success" (S9). Re-verified 2026-07-29
// against a full SENTINEL_E2E run: the in-process buffer this test exercised
// no longer exists at all (apps/processor-go/degradation/buffer.go's package
// doc comment), so there is no "full" state to reach and the test's premise
// is gone, not merely renamed.
//
// What replaces it: the actual current invariant is that ProcessEvent must
// NEVER return nil while the database is unavailable, no matter how many
// events are evaluated — an "Ack" with nothing stored is exactly the S9
// failure mode. This asserts that invariant holds across many repeated
// calls (previously it only held up to the buffer's capacity, then flipped).
func TestProcessorService_ProcessEvent_NeverSilentlyDropsWhenDBUnavailable(t *testing.T) {
	badCfg, err := pgxpool.ParseConfig(
		"postgres://sentinel:changeme@127.0.0.1:1/sentinel?sslmode=disable&connect_timeout=2",
	)
	require.NoError(t, err)
	badPool, err := pgxpool.NewWithConfig(context.Background(), badCfg)
	require.NoError(t, err)
	t.Cleanup(badPool.Close)

	svc := service.NewProcessorService(badPool)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Comfortably more than the old MaxBufferSize's failure point would have
	// needed (10000) is not required anymore since there is no capacity to
	// exhaust; a few hundred calls is enough to prove "never nil" as a
	// standing invariant rather than a coincidence of the first call.
	const iterations = 500
	for i := 0; i < iterations; i++ {
		ev, mErr := proto.Marshal(buildValidProtoEvent(uniqueProjectName("u15-never-drop")))
		require.NoError(t, mErr)
		callErr := svc.ProcessEvent(ctx, ev)
		require.Error(t, callErr,
			"call %d: ProcessEvent must never return nil while the database is unavailable — a nil return acks the NATS message with nothing stored (S9)", i)
		assert.ErrorContains(t, callErr, "database unavailable")
	}
}
